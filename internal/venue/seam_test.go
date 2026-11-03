package venue

// The seam the pipeline crosses: a worker RESOLVED through the leases is handed to a runner, and the runner has to dial that machine. The halves are each covered elsewhere (the lease resolves, the dialer connects); this is the test that carries one across the other.

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue/iapdial"
)

// A launch-rung worker resolved through the real leases, handed to NewRunner, is dialed at the instance the acquirer created: not the template, not nothing, and not whatever a re-read of the mapping would name.
func TestResolvedWorkerIsTheMachineTheRunnerDials(t *testing.T) {
	shrinkGCPWaits(t)

	sshd := newGCPSSHD(t)
	fake := &fakeGCE{}
	dialed := seamGCP(t, fake, sshd)

	spec := localGCPWorker(t, sshd, t.TempDir())

	worker := mustParse(t, "gcp://launch/steps-workers?project=test-project&zone=us-central1-a&idle=3m")
	leases := NewLeases(map[string]Worker{"gpu": worker})

	resolved, err := leases.Resolve(context.Background(), "gpu")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	spec.WorkerTag = "gpu"
	runner := newRunnerOn(t, resolved, spec.RunnerSpec)

	err = shell.Run(context.Background(), runner, "true")
	if err != nil {
		t.Fatalf("Run on the resolved worker: %v", err)
	}

	fake.mu.Lock()
	created := strings.TrimPrefix(fake.inserts[0], "steps-workers->")
	fake.mu.Unlock()

	want := iapdial.Target{Project: "test-project", Zone: "us-central1-a", Instance: created, Port: 22}
	if got := dialed.last(t); got != want {
		t.Errorf("the relay was asked for %+v, want the machine the leases resolved: %+v", got, want)
	}

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
}

// The holder a placed step records for the trees it keeps is the URL of the machine the leases resolved, parsed back and dialed by whichever step pulls from it: it has to name the launched instance with its project and zone, not the template.
func TestHolderOfAResolvedWorkerPullsFromTheMachineItAcquired(t *testing.T) {
	shrinkGCPWaits(t)

	sshd := newGCPSSHD(t)
	fake := &fakeGCE{}
	dialed := seamGCP(t, fake, sshd)

	spec := localGCPWorker(t, sshd, filepath.Join(t.TempDir(), "src"))
	mustMkdir(t, spec.Cwd)

	worker := mustParse(t, "gcp://launch/steps-workers?project=test-project&zone=us-central1-a&idle=3m")
	leases := NewLeases(map[string]Worker{"gpu": worker})

	resolved, err := leases.Resolve(context.Background(), "gpu")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	spec.FetchAll, spec.DeferFetch, spec.HoldAs, spec.WorkerTag = true, true, "src", "gpu"
	runner := newRunnerOn(t, resolved, spec.RunnerSpec)

	err = shell.Run(context.Background(), runner, "mkdir d && echo deep > d/f")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	held, holder, ok := HeldOf(runner)
	if !ok || held["src"] == "" {
		t.Fatalf("HeldOf = %v, want the tree held under its artifact name", held)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	dst := t.TempDir()

	_, err = Pull(context.Background(), holder, held["src"], dst)
	if err != nil {
		t.Fatalf("pulling from holder %q: %v", holder, err)
	}

	if got := mustRead(t, filepath.Join(dst, "d", "f")); got != "deep\n" {
		t.Errorf("pulled d/f = %q, want the tree the step kept", got)
	}

	fake.mu.Lock()
	created := strings.TrimPrefix(fake.inserts[0], "steps-workers->")
	fake.mu.Unlock()

	want := iapdial.Target{Project: "test-project", Zone: "us-central1-a", Instance: created, Port: 22}
	if got := dialed.last(t); got != want {
		t.Errorf("the pull asked the relay for %+v, want the machine the leases resolved: %+v", got, want)
	}

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
}

// Abandon forgets the machine its caller watched die and nothing else: a stale notice, naming a machine since replaced under the same tag, must not orphan the fresh one a sibling re-acquired.
func TestAbandonRetiresOnlyTheMachineItNames(t *testing.T) {
	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	leases := registry.Leases(boxWorker(t, "aws://launch/lt-0def4567890abcde"))
	ctx := context.Background()

	stale := mustResolveTag(ctx, t, leases, "box")

	leases.Abandon("box", stale)

	fresh := mustResolveTag(ctx, t, leases, "box")
	if fresh.Instance == stale.Instance {
		t.Fatalf("Abandon of %s left the tag on the same machine", stale.Instance)
	}

	leases.Abandon("box", stale)

	if again := mustResolveTag(ctx, t, leases, "box"); again.Instance != fresh.Instance {
		t.Errorf("a stale Abandon retired the fresh machine: resolved %s, want %s kept", again.Instance, fresh.Instance)
	}

	leases.Abandon("box", fresh)

	if after := mustResolveTag(ctx, t, leases, "box"); after.Instance == fresh.Instance {
		t.Errorf("Abandon of the current machine %s kept it", fresh.Instance)
	}

	acquired, _ := fake.counts()
	if acquired != 3 {
		t.Errorf("%d machines acquired, want 3: stale, fresh, then the replacement of fresh", acquired)
	}

	_ = leases.ReleaseAll(ctx)
	_ = registry.Close(ctx)
}

// The zero Worker is this machine: shell's own runner, untouched, and a refusal for inputs only another worker could have offered.
func TestZeroWorkerIsThisMachine(t *testing.T) {
	t.Parallel()

	spec := shell.RunnerSpec{Cwd: t.TempDir()}

	here, err := NewRunner(Worker{}, spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer func() { _ = here.Close() }()

	direct, err := shell.NewRunner(spec)
	if err != nil {
		t.Fatalf("shell.NewRunner: %v", err)
	}

	defer func() { _ = direct.Close() }()

	if reflect.TypeOf(here) != reflect.TypeOf(direct) {
		t.Errorf("the zero Worker built a %T, want shell's own %T", here, direct)
	}

	spec.RemoteInputs = map[string]shell.RemoteInput{"repo": {}}

	_, err = NewRunner(Worker{}, spec)
	if !errors.Is(err, errRemoteInputsHere) {
		t.Errorf("remote inputs with no worker: %v, want errRemoteInputsHere", err)
	}
}
