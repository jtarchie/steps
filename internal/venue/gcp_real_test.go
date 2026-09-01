package venue

// The gcp:// venue against real GCP — the tests the fakes cannot replace.
//
// internal/venue/gcp_test.go fakes the control plane and seams out the
// tunnel, so it proves the wiring but nothing about Google itself. These
// prove the rest: that the relay accepts our websocket and carries an SSH
// session, that a minted metadata key authenticates against a real guest
// agent, that guest attributes really do attest host keys, that
// instances.insert from a template means what we think — and, the one
// nothing else can reach, that a real preemption travels metadata →
// draining frame → eviction.
//
// Opt-in, and skipped hermetically without the fixture:
//
//	hack/gcp-fixture.sh up      # prints the environment to export
//	go test ./internal/venue -run TestRealGCP -v
//	hack/gcp-fixture.sh down
//
// Slow by nature — acquisition is tens of seconds and a preemption notice
// about thirty — so every test here budgets minutes, not seconds.

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

// gcpFixture is what hack/gcp-fixture.sh exported, or a skip.
type gcpFixture struct {
	project  string
	zone     string
	instance string
	template string
	binary   string
}

func realGCP(t *testing.T) gcpFixture {
	t.Helper()

	fixture := gcpFixture{
		project:  os.Getenv("STEPS_TEST_GCP_PROJECT"),
		zone:     os.Getenv("STEPS_TEST_GCP_ZONE"),
		instance: os.Getenv("STEPS_TEST_GCP_INSTANCE"),
		template: os.Getenv("STEPS_TEST_GCP_TEMPLATE"),
		binary:   os.Getenv("STEPS_TEST_GCP_BINARY"),
	}

	if fixture.project == "" || fixture.zone == "" || fixture.instance == "" || fixture.binary == "" {
		t.Skip("no GCP fixture — run hack/gcp-fixture.sh up and export what it prints")
	}

	_, err := os.Stat(fixture.binary)
	if err != nil {
		t.Fatalf("the worker binary %s is missing: %v", fixture.binary, err)
	}

	return fixture
}

// options is the query every fixture worker URL carries.
func (f gcpFixture) options() string {
	return "project=" + f.project + "&zone=" + f.zone + "&binary=" + f.binary
}

func (f gcpFixture) spec(cwd, worker string, outputs ...string) shell.RunnerSpec {
	sep := "?"
	if strings.Contains(worker, "?") {
		sep = "&"
	}

	return shell.RunnerSpec{
		Cwd:       cwd,
		Worker:    worker + sep + f.options(),
		WorkerTag: "gcp",
		Fetch:     outputs,
	}
}

// TestRealGCPRunsAStepOnAnInstance is the whole gcp:// path against reality:
// a key minted and installed through real metadata, host keys read from real
// guest attributes, the real relay carrying a real SSH session, the binary
// pushed over sftp inside it, and a step's tree there and back — with no
// artifact store at all, which is itself part of what is being proven.
func TestRealGCPRunsAStepOnAnInstance(t *testing.T) {
	fixture := realGCP(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, fixture.spec(cwd, "gcp://"+fixture.instance+"/var/tmp/steps", "out"))

	err := runner.Run(ctx, "cat data/seed.txt > out/report.txt; uname -m >> out/report.txt")
	if err != nil {
		t.Fatalf("running a step on %s: %v", fixture.instance, err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if !strings.HasPrefix(got, "seed\n") {
		t.Errorf("out/report.txt = %q, want the input the worker consumed", got)
	}

	// The fixture is x86, so on an arm64 dev machine this also proves the
	// ?binary= foreign-arch path.
	if !strings.Contains(got, "x86_64") {
		t.Errorf("out/report.txt = %q, want it to name the worker's architecture", got)
	}
}

// TestRealGCPNonzeroExitIsAStepFailure pins the classification every hook
// depends on, over the real transport.
func TestRealGCPNonzeroExitIsAStepFailure(t *testing.T) {
	fixture := realGCP(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	runner := newLocalRunner(t, fixture.spec(t.TempDir(), "gcp://"+fixture.instance+"/var/tmp/steps"))

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

// TestRealGCPLaunchRungAcquiresAndDeletes settles what no fake can: that our
// instances.insert-from-template request is one Compute Engine actually
// accepts, that the machine it produces boots into something dialable, and
// that release genuinely deletes it.
func TestRealGCPLaunchRungAcquiresAndDeletes(t *testing.T) {
	fixture := realGCP(t)

	if fixture.template == "" {
		t.Skip("no STEPS_TEST_GCP_TEMPLATE in the fixture")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	worker, err := ParseWorker("gcp://launch/" + fixture.template + "/var/tmp/steps?" + fixture.options())
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

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
		t.Skipf("could not acquire a worker (no spot capacity, or the account forbids it): %v", err)
	}

	t.Logf("launched %s", resolved.Instance)

	runner := newLocalRunner(t, shell.RunnerSpec{
		Cwd:       t.TempDir(),
		Worker:    resolved.URL,
		WorkerTag: "burst",
	})

	err = runner.Run(ctx, "true")
	if err != nil {
		t.Fatalf("running on the launched instance: %v", err)
	}

	err = leases.ReleaseAll(ctx)
	if err != nil {
		t.Fatalf("deleting the launched instance: %v", err)
	}

	awaitInstanceGone(ctx, t, fixture, resolved.Instance)
}

// awaitInstanceGone polls until the API denies the instance exists — the
// answer a release claims to have arranged.
func awaitInstanceGone(ctx context.Context, t *testing.T, fixture gcpFixture, name string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Minute)

	for {
		// Output, not CombinedOutput: gcloud warns on stderr when a filter
		// matches nothing — which is exactly the empty answer this waits for,
		// and reading the warning as output made "gone" unrecognizable.
		out, listErr := exec.CommandContext(ctx, "gcloud", "compute", "instances", "list", //nolint:gosec // fixed argv over fixture names
			"--project", fixture.project, "--zones", fixture.zone,
			"--filter=name="+name, "--format=value(name)").Output()
		if listErr == nil && strings.TrimSpace(string(out)) == "" {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("instance %s still exists after release: %s", name, out)
		}

		time.Sleep(10 * time.Second)
	}
}

// TestRealGCPParkedRungStartsAndStops proves the stopped rung against the
// real lifecycle, including Compute Engine's TERMINATED-means-parked wart.
func TestRealGCPParkedRungStartsAndStops(t *testing.T) {
	fixture := realGCP(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	gcloudCLI(ctx, t, fixture, "compute", "instances", "stop", fixture.instance)

	worker, err := ParseWorker("gcp://stopped/" + fixture.instance + "/var/tmp/steps?idle=0&" + fixture.options())
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

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

	// The release returns once the stop is ACCEPTED; starting again while
	// the instance is still STOPPING loses a fingerprint race inside GCE
	// ("the resource fingerprint changed during the start operation" —
	// observed). Wait for parked before the courtesy restart.
	awaitInstanceStatus(ctx, t, fixture, fixture.instance, "TERMINATED")

	// Left running for whatever runs next, since every other test wants it up.
	gcloudCLI(ctx, t, fixture, "compute", "instances", "start", fixture.instance)
}

// awaitInstanceStatus polls until the instance reports one status.
func awaitInstanceStatus(ctx context.Context, t *testing.T, fixture gcpFixture, name, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Minute)

	for {
		out, err := exec.CommandContext(ctx, "gcloud", "compute", "instances", "describe", name, //nolint:gosec // fixed argv over fixture names
			"--project", fixture.project, "--zone", fixture.zone,
			"--format=value(status)").Output()
		if err == nil && strings.TrimSpace(string(out)) == want {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("instance %s never reached %s (last: %s)", name, want, strings.TrimSpace(string(out)))
		}

		time.Sleep(10 * time.Second)
	}
}

// TestRealGCPPreemption is the one this whole fixture exists for.
//
// simulateMaintenanceEvent on a spot instance is Google's own way to trigger
// a real preemption — the docs frame it as exactly this test. The metadata
// flag flips, our shim's watcher sees it, relays a draining frame, and the
// orchestrator re-reads the failure that follows as infrastructure rather
// than the step's verdict. Every link has a fake; none of the fakes can
// prove the simulated path matches the real one — which is precisely the
// open question this test answers (a community report suggests the simulated
// shutdown may skip steps a real reclaim takes).
func TestRealGCPPreemption(t *testing.T) {
	fixture := realGCP(t)

	if fixture.template == "" {
		t.Skip("no STEPS_TEST_GCP_TEMPLATE in the fixture")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// The fixture template is spot with DELETE — the shape a preemption test
	// needs, and the reason release tolerates an instance that removed
	// itself.
	worker, err := ParseWorker("gcp://launch/" + fixture.template + "/var/tmp/steps?" + fixture.options())
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

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
		t.Skipf("could not acquire a spot worker (no capacity, or the account forbids it): %v", err)
	}

	t.Logf("spot instance %s — preempting it", resolved.Instance)

	runner := newLocalRunner(t, shell.RunnerSpec{
		Cwd:       t.TempDir(),
		Worker:    resolved.URL,
		WorkerTag: "spot",
	})

	// Open the session first: the watcher only polls while a session is
	// alive, so the preemption has to land on a running conversation.
	err = runner.Run(ctx, "true")
	if err != nil {
		t.Fatalf("opening a session on the spot instance: %v", err)
	}

	gcloudCLI(ctx, t, fixture, "compute", "instances", "simulate-maintenance-event",
		resolved.Instance, "--async")

	// GCE's warning is about thirty seconds and the shim polls every five —
	// so a command started now should hear the notice mid-run.
	err = runner.Run(ctx, "sleep 240")
	if err == nil {
		t.Fatal("the command outlived the preemption — the instance was never reclaimed")
	}

	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("error = %v, want ErrEvicted — a real preemption was read as something else", err)
	}

	t.Logf("preemption classified correctly: %v", err)
}

// gcloudCLI shells out rather than widening the compute client: these calls
// are fixture management, not product code.
func gcloudCLI(ctx context.Context, t *testing.T, fixture gcpFixture, args ...string) {
	t.Helper()

	args = append(args, "--project", fixture.project, "--zone", fixture.zone, "--quiet")

	out, err := exec.CommandContext(ctx, "gcloud", args...).CombinedOutput() //nolint:gosec // fixed argv this test built
	if err != nil {
		t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
