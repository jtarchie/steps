package venue

// The aws:// venue against real AWS — the tests the fakes cannot replace.
//
// internal/venue/ssm_test.go fakes the control plane and seams out the data
// channel, so it proves the bootstrap script and the wiring but nothing about
// AWS itself. These prove the rest: that the script runs on a real AMI, that
// the SSM agent accepts our session, that a real EC2 fleet request means what
// we think it means, and — the one nothing else can reach — that a real spot
// reclamation travels IMDS → draining frame → re-placement.
//
// Opt-in, and skipped hermetically without the fixture:
//
//	hack/aws-fixture.sh up      # prints the environment to export
//	go test ./internal/venue -run TestRealAWS -v
//	hack/aws-fixture.sh down
//
// Slow by nature — acquisition is 20-90 seconds and a spot interruption
// notice is two minutes — so every test here budgets minutes, not seconds.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// awsFixture is what hack/aws-fixture.sh exported, or a skip.
type awsFixture struct {
	instance string
	template string
	bucket   string
	binary   string
	region   string
	fisRole  string
}

func realAWS(t *testing.T) awsFixture {
	t.Helper()

	fixture := awsFixture{
		instance: os.Getenv("STEPS_TEST_AWS_INSTANCE"),
		template: os.Getenv("STEPS_TEST_AWS_TEMPLATE"),
		bucket:   os.Getenv("STEPS_TEST_AWS_BUCKET"),
		binary:   os.Getenv("STEPS_TEST_AWS_BINARY"),
		region:   os.Getenv("STEPS_TEST_AWS_REGION"),
		fisRole:  os.Getenv("STEPS_TEST_AWS_FIS_ROLE"),
	}

	if fixture.instance == "" || fixture.bucket == "" || fixture.binary == "" {
		t.Skip("no AWS fixture — run hack/aws-fixture.sh up and export what it prints")
	}

	_, err := os.Stat(fixture.binary)
	if err != nil {
		t.Fatalf("the worker binary %s is missing: %v", fixture.binary, err)
	}

	return fixture
}

// store is the --artifact-store URL for the fixture bucket.
func (f awsFixture) store() string {
	url := "s3://" + f.bucket + "/steps-test"
	if f.region != "" {
		url += "?region=" + f.region
	}

	return url
}

// spec builds a RunnerSpec pointing at a worker URL, with the fixture's
// binary pushed through the fixture's artifact store.
func (f awsFixture) spec(cwd, worker string, outputs ...string) shell.RunnerSpec {
	sep := "?"
	if strings.Contains(worker, "?") {
		sep = "&"
	}

	return shell.RunnerSpec{
		Cwd:           cwd,
		Worker:        worker + sep + "binary=" + f.binary,
		WorkerTag:     "aws",
		Fetch:         outputs,
		ArtifactStore: f.store(),
	}
}

// TestRealAWSRunsAStepOnAManagedInstance is the whole aws:// path against
// reality: the bootstrap command runs on a real Amazon Linux AMI, fetches the
// binary from a real presigned URL, starts a shim on loopback, and a real SSM
// port-forwarding session carries the protocol to it — with the step's tree
// travelling through real S3 rather than the tunnel.
//
// Every layer this exercises has a fake elsewhere. This is the only place all
// of them are the real thing at once.
func TestRealAWSRunsAStepOnAManagedInstance(t *testing.T) {
	fixture := realAWS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, fixture.spec(cwd, "aws://"+fixture.instance, "out"))

	err := runner.Run(ctx, "cat data/seed.txt > out/report.txt; uname -m >> out/report.txt")
	if err != nil {
		t.Fatalf("running a step on %s: %v", fixture.instance, err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if !strings.HasPrefix(got, "seed\n") {
		t.Errorf("out/report.txt = %q, want the input the worker consumed", got)
	}

	// The fixture is Graviton, so this also proves the ?binary= foreign-arch
	// path: a binary this machine cannot run, pushed and executed there.
	if !strings.Contains(got, "aarch64") {
		t.Errorf("out/report.txt = %q, want it to name the worker's architecture", got)
	}
}

// TestRealAWSNonzeroExitIsAStepFailure pins the classification that every
// hook depends on, over the real transport: a command that ran and failed is
// a failed step, not broken machinery.
func TestRealAWSNonzeroExitIsAStepFailure(t *testing.T) {
	fixture := realAWS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	runner := newLocalRunner(t, fixture.spec(t.TempDir(), "aws://"+fixture.instance))

	err := runner.Run(ctx, "exit 7")
	if err == nil {
		t.Fatal("a failing command on a real worker reported success")
	}

	if !shell.IsExitError(err) {
		t.Fatalf("error = %v, want the command's own exit rather than infrastructure", err)
	}

	if exitCodeOf(err) != 7 {
		t.Errorf("exit code = %d, want 7", exitCodeOf(err))
	}
}

// TestRealAWSLaunchRungAcquiresAndTerminates settles what no fake can: that
// our CreateFleet request is one EC2 actually accepts, and that the instance
// it produces is dialable and destroyed afterwards.
//
// The fleet shape is the part that was a guess — an omitted capacity type,
// spot-with-fallback in one call — and a fake EC2 could only agree with the
// guess.
func TestRealAWSLaunchRungAcquiresAndTerminates(t *testing.T) {
	fixture := realAWS(t)

	if fixture.template == "" {
		t.Skip("no STEPS_TEST_AWS_TEMPLATE in the fixture")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	worker, err := ParseWorker("aws://launch/" + fixture.template + "?capacity=spot-then-od&binary=" + fixture.binary)
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	worker.ArtifactStore = fixture.store()

	leases := NewLeases(map[string]Worker{"burst": worker})

	t.Cleanup(func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer releaseCancel()

		releaseErr := leases.ReleaseAll(releaseCtx)
		if releaseErr != nil {
			t.Errorf("releasing the launched worker: %v — CHECK FOR A RUNNING INSTANCE", releaseErr)
		}
	})

	resolved, err := leases.Resolve(ctx, "burst")
	if err != nil {
		t.Fatalf("acquiring a launched worker: %v", err)
	}

	if resolved.Instance == "" || resolved.Rung != RungStatic {
		t.Fatalf("resolved = %+v, want a running instance", resolved)
	}

	t.Logf("launched %s", resolved.Instance)

	// The URL a runner re-parses has to dial the machine that was launched —
	// the break no reviewer caught, worth proving against reality.
	runner := newLocalRunner(t, shell.RunnerSpec{
		Cwd:           t.TempDir(),
		Worker:        resolved.URL,
		ArtifactStore: fixture.store(),
	})

	err = runner.Run(ctx, "true")
	if err != nil {
		t.Fatalf("dialling the launched instance %s: %v", resolved.Instance, err)
	}
}

// TestRealAWSParkedRungStartsAndStops covers the stopped rung against real
// EC2 state transitions. It stops the fixture instance first and restarts it
// through the lease, leaving it running for the other tests.
func TestRealAWSParkedRungStartsAndStops(t *testing.T) {
	fixture := realAWS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	awsCLI(ctx, t, "ec2", "stop-instances", "--instance-ids", fixture.instance)
	awsCLI(ctx, t, "ec2", "wait", "instance-stopped", "--instance-ids", fixture.instance)

	worker, err := ParseWorker("aws://stopped/" + fixture.instance + "?idle=0&binary=" + fixture.binary)
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	worker.ArtifactStore = fixture.store()

	leases := NewLeases(map[string]Worker{"parked": worker})

	resolved, err := leases.Resolve(ctx, "parked")
	if err != nil {
		t.Fatalf("starting the parked worker: %v", err)
	}

	if resolved.Instance != fixture.instance {
		t.Errorf("resolved instance = %q, want the parked one", resolved.Instance)
	}

	err = leases.ReleaseAll(ctx)
	if err != nil {
		t.Fatalf("parking it again: %v", err)
	}

	awsCLI(ctx, t, "ec2", "wait", "instance-stopped", "--instance-ids", fixture.instance)

	// Left running for whatever runs next, since every other test wants it up.
	awsCLI(ctx, t, "ec2", "start-instances", "--instance-ids", fixture.instance)
	awsCLI(ctx, t, "ec2", "wait", "instance-running", "--instance-ids", fixture.instance)
}

// TestRealAWSSpotEviction is the one this whole fixture exists for.
//
// A spot reclamation is the only way to prove the chain end to end: the SSM
// agent's own metadata service publishes the notice, our shim's IMDS watcher
// sees it, relays a draining frame, and the orchestrator re-reads the failure
// that follows as infrastructure rather than as the step's verdict. Every
// link has a fake; none of the fakes can produce a real interruption.
//
// FIS delivers the genuine notice — the same one AWS's own console button
// sends. It costs about $0.10 per action-minute.
func TestRealAWSSpotEviction(t *testing.T) {
	fixture := realAWS(t)

	if fixture.template == "" || fixture.fisRole == "" {
		t.Skip("no STEPS_TEST_AWS_TEMPLATE / STEPS_TEST_AWS_FIS_ROLE in the fixture")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Spot only: FIS refuses to interrupt an on-demand instance, so a
	// fallback here would silently make the test vacuous.
	worker, err := ParseWorker("aws://launch/" + fixture.template + "?capacity=spot&binary=" + fixture.binary)
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	worker.ArtifactStore = fixture.store()

	leases := NewLeases(map[string]Worker{"spot": worker})

	t.Cleanup(func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer releaseCancel()

		releaseErr := leases.ReleaseAll(releaseCtx)
		if releaseErr != nil {
			t.Errorf("releasing the spot worker: %v — CHECK FOR A RUNNING INSTANCE", releaseErr)
		}
	})

	resolved, err := leases.Resolve(ctx, "spot")
	if err != nil {
		t.Skipf("no spot capacity for the fixture template right now: %v", err)
	}

	t.Logf("spot instance %s — interrupting it", resolved.Instance)

	runner := newLocalRunner(t, shell.RunnerSpec{
		Cwd:           t.TempDir(),
		Worker:        resolved.URL,
		ArtifactStore: fixture.store(),
	})

	// Open the session first: the watcher only polls while a session is
	// alive, so the interruption has to land on a running conversation.
	err = runner.Run(ctx, "true")
	if err != nil {
		t.Fatalf("opening a session on the spot instance: %v", err)
	}

	interrupt(ctx, t, fixture, resolved.Instance)

	// The notice reaches IMDS about two minutes before the machine goes, and
	// the shim polls every five seconds — so a command started now should
	// meet a session that has already heard, or hear it mid-run.
	err = runner.Run(ctx, "sleep 240")
	if err == nil {
		t.Fatal("the command outlived the interruption — the instance was never reclaimed")
	}

	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("error = %v, want ErrEvicted — a real reclamation was read as something else", err)
	}

	t.Logf("eviction classified correctly: %v", err)
}

// interrupt asks FIS to reclaim a spot instance for real.
func interrupt(ctx context.Context, t *testing.T, fixture awsFixture, instance string) {
	t.Helper()

	template := `{
      "description": "steps test spot interruption",
      "roleArn": "` + fixture.fisRole + `",
      "stopConditions": [{"source": "none"}],
      "targets": {
        "victim": {
          "resourceType": "aws:ec2:spot-instance",
          "resourceArns": ["arn:aws:ec2:` + fixture.region + `:` + accountID(ctx, t) + `:instance/` + instance + `"],
          "selectionMode": "ALL"
        }
      },
      "actions": {
        "interrupt": {
          "actionId": "aws:ec2:send-spot-instance-interruptions",
          "parameters": {"durationBeforeInterruption": "PT2M"},
          "targets": {"SpotInstances": "victim"}
        }
      }
    }`

	file := filepath.Join(t.TempDir(), "fis.json")

	err := os.WriteFile(file, []byte(template), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	out := awsCLI(ctx, t, "fis", "create-experiment-template", "--cli-input-json", "file://"+file,
		"--query", "experimentTemplate.id", "--output", "text")

	id := strings.TrimSpace(out)

	//nolint:contextcheck // deliberately its own context: the test's is spent by then
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()

		_ = exec.CommandContext(cleanupCtx, "aws", "fis", "delete-experiment-template", "--id", id).Run() //nolint:gosec // ids this test just minted
	})

	awsCLI(ctx, t, "fis", "start-experiment", "--experiment-template-id", id)
}

func accountID(ctx context.Context, t *testing.T) string {
	t.Helper()

	return strings.TrimSpace(awsCLI(ctx, t, "sts", "get-caller-identity", "--query", "Account", "--output", "text"))
}

// awsCLI shells out rather than importing another SDK: these calls are
// fixture management, not product code, and adding service/fis to go.mod for
// a test-only path would put it in every build.
func awsCLI(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()

	if region := os.Getenv("STEPS_TEST_AWS_REGION"); region != "" {
		args = append([]string{"--region", region}, args...)
	}

	out, err := exec.CommandContext(ctx, "aws", args...).CombinedOutput() //nolint:gosec // fixed argv this test built
	if err != nil {
		t.Fatalf("aws %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return string(out)
}
