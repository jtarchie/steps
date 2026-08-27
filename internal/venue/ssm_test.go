package venue

// The aws:// venue, against a fake control plane.
//
// The fake stands in for SSM itself: SendCommand runs the bootstrap script
// with a real shell on this machine, and the forwarded session is a real TCP
// connection to the port that script reported. So everything except AWS is
// real — a real script, a real shim listening on a real port, a real venue
// session — and what is faked is exactly the part that needs an AWS account.
//
// The data channel is seamed out rather than faked here: it has its own tests
// one package over, against an agent that speaks the real protocol, and
// running it twice would test the fake rather than the venue.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue/ssmdial"
)

// fakeSSM is the control plane: it runs bootstrap scripts locally and
// forwards sessions to whatever port they reported.
type fakeSSM struct {
	t *testing.T

	// platform is what DescribeInstanceInformation reports.
	platform ssmtypes.PlatformType
	// unmanaged makes DescribeInstanceInformation report nothing, the shape
	// of an instance SSM cannot reach.
	unmanaged bool
	// unmanagedBefore is how many answers report nothing before the agent
	// registers — a freshly launched instance takes 1-3 minutes to appear.
	unmanagedBefore int

	mu        sync.Mutex
	described int
	scripts   []string
	output    string
	failed    bool
	commands  int
}

func (f *fakeSSM) DescribeInstanceInformation(
	_ context.Context, _ *ssm.DescribeInstanceInformationInput, _ ...func(*ssm.Options),
) (*ssm.DescribeInstanceInformationOutput, error) {
	f.mu.Lock()
	f.described++
	described := f.described
	f.mu.Unlock()

	if f.unmanaged || described <= f.unmanagedBefore {
		return &ssm.DescribeInstanceInformationOutput{}, nil
	}

	platform := f.platform
	if platform == "" {
		platform = ssmtypes.PlatformTypeLinux
	}

	return &ssm.DescribeInstanceInformationOutput{
		InstanceInformationList: []ssmtypes.InstanceInformation{{PlatformType: platform}},
	}, nil
}

// SendCommand runs the bootstrap for real, with sh, on this machine. That is
// the point: the script is the part of this feature most likely to be wrong,
// and a fake that only pretended to run it would prove nothing.
func (f *fakeSSM) SendCommand(
	ctx context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options),
) (*ssm.SendCommandOutput, error) {
	script := in.Parameters["commands"][0]

	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	f.commands++
	f.mu.Unlock()

	out, err := exec.CommandContext(ctx, "sh", "-c", script).CombinedOutput() //nolint:gosec // the script under test

	f.mu.Lock()
	f.output = string(out)
	f.failed = err != nil
	f.mu.Unlock()

	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("c-1")}}, nil
}

func (f *fakeSSM) GetCommandInvocation(
	_ context.Context, _ *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options),
) (*ssm.GetCommandInvocationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	status := ssmtypes.CommandInvocationStatusSuccess
	if f.failed {
		status = ssmtypes.CommandInvocationStatusFailed
	}

	return &ssm.GetCommandInvocationOutput{
		Status:                status,
		StandardOutputContent: aws.String(f.output),
		StandardErrorContent:  aws.String(f.output),
	}, nil
}

// StartSession is never reached: these tests seam ssmForward instead, so the
// forwarded session is a plain TCP dial to the port the bootstrap reported.
// The data channel that would carry it has its own tests one package over,
// against a fake agent that speaks the real protocol.
func (f *fakeSSM) StartSession(
	_ context.Context, _ *ssm.StartSessionInput, _ ...func(*ssm.Options),
) (*ssm.StartSessionOutput, error) {
	return nil, errNotForwardedHere
}

// errNotForwardedHere marks the seam these tests deliberately do not cross.
var errNotForwardedHere = errors.New("StartSession is seamed out in venue tests; see ssmForward")

// localSSMWorker builds a spec pointing at an aws:// worker whose bootstrap
// starts this test binary as a shim.
func localSSMWorker(t *testing.T, fake *fakeSSM, cwd string, outputs ...string) shell.RunnerSpec {
	t.Helper()

	fake.t = t

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	seamSSM(t, fake)

	// ?shim= names a binary already on the "instance", which here is this
	// test binary — so the bootstrap script runs for real without needing an
	// artifact store to fetch from.
	return shell.RunnerSpec{
		Cwd:    cwd,
		Worker: "aws://i-0abc123def456789?shim=" + self,
		Fetch:  outputs,
	}
}

// seamSSM points aws:// dials at the fake control plane, and forwards
// sessions by dialling the port the bootstrap actually reported — a real TCP
// connection to a real shim, with only AWS itself replaced.
func seamSSM(t *testing.T, fake *fakeSSM) {
	t.Helper()

	fake.t = t

	previousAPI, previousForward := ssmAPIFor, ssmForward

	ssmAPIFor = func(context.Context, Worker) (ssmdial.API, error) { return fake, nil }
	ssmForward = func(ctx context.Context, _ ssmdial.API, _ string, port int) (io.ReadWriteCloser, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return nil, fmt.Errorf("reaching the bootstrapped shim: %w", err)
		}

		return conn, nil
	}

	t.Cleanup(func() { ssmAPIFor, ssmForward = previousAPI, previousForward })
}

// TestVenueRunsAStepOnAnSSMWorker is the feature: a step placed on an aws://
// worker is bootstrapped through the control plane, reached over a forwarded
// session, and gives its outputs back — the same contract every other venue
// meets, over a transport with no inbound port.
func TestVenueRunsAStepOnAnSSMWorker(t *testing.T) {
	fake := &fakeSSM{}

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localSSMWorker(t, fake, cwd, "out"))

	err := runner.Run(context.Background(), "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("Run on an aws:// worker: %v", err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if got != "seed\n" {
		t.Errorf("out/report.txt = %q, want %q", got, "seed\n")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.commands != 1 {
		t.Errorf("the venue sent %d bootstrap commands, want 1 for one session", fake.commands)
	}

	// The listener is loopback, which is the property that makes an aws://
	// worker need no inbound port at all.
	if !strings.Contains(fake.scripts[0], "127.0.0.1:0") {
		t.Errorf("the bootstrap did not bind loopback with a chosen port:\n%s", fake.scripts[0])
	}

	if !strings.Contains(fake.scripts[0], "--once") {
		t.Errorf("the bootstrap left a shim that outlives its session:\n%s", fake.scripts[0])
	}

	// --once only ends a shim somebody DIALLED. The dial happens after this
	// script returns, so a failure in between — SSM throttling the session, a
	// websocket that will not open — leaves a root process in Accept holding
	// a port, with nothing on this end still referring to it.
	if !strings.Contains(fake.scripts[0], "--linger") {
		t.Errorf("the bootstrap left a shim nothing would ever reap:\n%s", fake.scripts[0])
	}
}

// TestVenueRefusesAWindowsSSMWorker pins that the refusal happens BEFORE a
// binary is fetched or a process started on someone's instance, even though
// the handshake would catch it either way.
func TestVenueRefusesAWindowsSSMWorker(t *testing.T) {
	fake := &fakeSSM{platform: ssmtypes.PlatformTypeWindows}

	runner := newLocalRunner(t, localSSMWorker(t, fake, t.TempDir()))

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("a Windows instance was accepted")
	}

	if !strings.Contains(err.Error(), "executable bit") {
		t.Errorf("error = %v, want the reason the filesystem cannot hold the tree", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.commands != 0 {
		t.Errorf("%d commands ran on a worker that was going to be refused", fake.commands)
	}
}

// TestVenueReportsAnInstanceSSMCannotReach pins the error an operator most
// often hits first: the instance profile is missing, so SSM has never heard
// of the machine.
func TestVenueReportsAnInstanceSSMCannotReach(t *testing.T) {
	shrinkRegisterWait(t)

	fake := &fakeSSM{unmanaged: true}

	runner := newLocalRunner(t, localSSMWorker(t, fake, t.TempDir()))

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("an unmanaged instance was accepted")
	}

	if !strings.Contains(err.Error(), "AmazonSSMManagedInstanceCore") {
		t.Errorf("error = %v, want the error to name what is missing", err)
	}
}

// TestParseAWSWorker pins the mapping grammar: what names an instance, and
// what can never work and is therefore refused when read.
func TestParseAWSWorker(t *testing.T) {
	t.Parallel()

	worker, err := ParseWorker("aws://i-0abc123def456789/mnt/fast?region=us-west-2&shim=/usr/local/bin/steps")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	if worker.Instance != "i-0abc123def456789" || worker.Root != "/mnt/fast" ||
		worker.Region != "us-west-2" || worker.Shim != "/usr/local/bin/steps" {
		t.Errorf("parsed = %+v, want the instance, root, region and shim path", worker)
	}

	if got := worker.Address(); got != "aws://i-0abc123def456789/mnt/fast" {
		t.Errorf("Address() = %q, want the instance and its root without the options", got)
	}

	for _, raw := range []string{
		"aws://not-an-instance",
		"aws://",
		"aws://i-0abc123def456789?shim=/usr/local/bin/steps&binary=/tmp/steps",
	} {
		_, err = ParseWorker(raw)
		if !errors.Is(err, ErrWorker) {
			t.Errorf("ParseWorker(%q) = %v, want ErrWorker", raw, err)
		}
	}
}

// TestSSMWorkerNeedsABinary pins that a mapping with no way to get a shim
// onto the instance says so, naming both answers.
func TestSSMWorkerNeedsABinary(t *testing.T) {
	fake := &fakeSSM{}
	seamSSM(t, fake)

	runner := newLocalRunner(t, shell.RunnerSpec{
		Cwd:    t.TempDir(),
		Worker: "aws://i-0abc123def456789",
	})

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("a worker with no shim binary was accepted")
	}

	if !strings.Contains(err.Error(), "?binary=") || !strings.Contains(err.Error(), "?shim=") {
		t.Errorf("error = %v, want both ways of supplying a binary named", err)
	}
}

// shrinkRegisterWait makes the agent-registration wait testable.
//
// The real bound is minutes, because that is how long amazon-ssm-agent takes
// to register a freshly launched instance. The branch worth proving is that
// the dial retries at all, not how long it is willing to.
func shrinkRegisterWait(t *testing.T) {
	t.Helper()

	previousTimeout, previousPoll := registerTimeout, registerPoll
	registerTimeout, registerPoll = 2*time.Second, 5*time.Millisecond

	t.Cleanup(func() { registerTimeout, registerPoll = previousTimeout, previousPoll })
}

// TestDialWaitsForTheSSMAgentToRegister pins the other half of what a real
// launch caught: an instance EC2 calls "running" has no SSM agent registered
// for another one to three minutes, and asking once fails every time.
//
// The comment on waitForRunning promised this wait — "the dial that follows
// waits for the agent on its own terms" — and nothing implemented it, so the
// launch rung could not have worked even once EC2 admitted the instance
// existed.
func TestDialWaitsForTheSSMAgentToRegister(t *testing.T) {
	shrinkRegisterWait(t)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")

	fake := &fakeSSM{unmanagedBefore: 2}
	runner := newLocalRunner(t, localSSMWorker(t, fake, cwd))

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.described < 3 {
		t.Errorf("described = %d, want the unregistered answers to have been retried", fake.described)
	}
}
