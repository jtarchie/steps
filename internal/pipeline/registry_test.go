package pipeline

// A borrowable venue that is not a cloud: acquiring it hands out this machine as a local: worker, and the counts are the point.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/venue"
	"github.com/jtarchie/steps/internal/workspace"
)

// borrowedWorker is a parked instance as far as every check is concerned; ?shim= satisfies the aws placement check, and the fake acquirer means nothing ever dials AWS.
const borrowedWorker = "aws://stopped/i-0abc123def456789?shim=/usr/local/bin/steps"

type borrowed struct {
	mu     sync.Mutex
	starts int
	stops  int
}

func (b *borrowed) acquire(context.Context, venue.Worker) (venue.Worker, func(context.Context) error, error) {
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()

	return venue.Worker{URL: "local:", Scheme: venue.SchemeLocal}, func(context.Context) error {
		b.mu.Lock()
		b.stops++
		b.mu.Unlock()

		return nil
	}, nil
}

func (b *borrowed) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.starts, b.stops
}

// borrowedRun is a pipeline, a store, a workspace and a context whose box tag names a borrowed machine.
func borrowedRun(t *testing.T, yaml string) (context.Context, *config.Config, workspace.Provider, store.Store, *borrowed) {
	t.Helper()

	// A registry bypassed by mistake acquires through the real EC2 client, and that must fail here rather than reach whatever account the shell has.
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	ctx, err := WithWorkers(context.Background(), map[string]string{"box": borrowedWorker})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	fake := &borrowed{}

	ctx, closeRegistry := withRegistry(ctx, venue.NewRegistryWith(fake.acquire))
	t.Cleanup(closeRegistry)

	return ctx, cfg, provider, st, fake
}

// The failure the registry exists for, end to end through RunJob: the job that finishes first used to stop the machine the other was mid-step on.
func TestTwoJobsShareOneBorrowedMachine(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")

	ctx, cfg, provider, st, fake := borrowedRun(t, fmt.Sprintf(`
jobs:
- name: long
  plan:
  - task: hold
    tags: [box]
    run: |
      touch %s
      until [ -f %s ]; do sleep 0.05; done
- name: short
  plan:
  - task: quick
    tags: [box]
    run: "true"
`, started, release))

	// However this test ends, the long job has to be let go or its goroutine outlives it.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	long := make(chan error, 1)

	go func() { long <- RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false) }()

	awaitFile(t, started)

	err := RunJob(ctx, cfg, &cfg.Jobs[1], nil, provider, st, false)
	if err != nil {
		t.Fatalf("short job: %v", err)
	}

	if starts, stops := fake.counts(); starts != 1 || stops != 0 {
		t.Fatalf("after the short job: %d starts, %d stops — want one machine, still up under the long job", starts, stops)
	}

	err = os.WriteFile(release, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = <-long
	if err != nil {
		t.Fatalf("long job: %v", err)
	}

	if starts, stops := fake.counts(); starts != 1 || stops != 1 {
		t.Fatalf("after both jobs: %d starts, %d stops — want it given back once, by the last user", starts, stops)
	}
}

// A job's pre-plan check on a borrowed machine runs; skipped, as it had to be while a check and a job could not share one, the second run builds the version history already had rather than the one upstream has now.
func TestAJobChecksAResourceOnABorrowedMachine(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	fetched := filepath.Join(dir, "fetched")

	ctx, cfg, provider, st, fake := borrowedRun(t, fmt.Sprintf(`
resource_types:
- name: probe
  config:
    check: printf '[{"ref":"%%s"}]' "$(cat %s)"
    in: echo {{ .version.ref }} >> %s

resources:
- name: repo
  type: probe
  tags: [box]
  source: {}

jobs:
- name: build
  plan:
  - get: repo
`, upstream, fetched))

	// What a poll left behind: history reads v1, while upstream has since moved on. With no history at all the get checks for itself, and the refresh would not be what decides.
	_, err := st.RecordVersions(context.Background(), "repo", []map[string]any{{"ref": "v1"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(upstream, []byte("v2"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	log, err := os.ReadFile(fetched) //nolint:gosec // a t.TempDir() file this test's pipeline wrote
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(string(log)); got != "v2" {
		t.Errorf("fetched %q, want v2 — the check was skipped and the run built what history had", got)
	}

	if starts, stops := fake.counts(); starts != 1 || stops != 1 {
		t.Errorf("%d starts, %d stops — want the check and the fetch sharing one machine, given back once", starts, stops)
	}

	err = ValidatePipelinePlacement(ctx, cfg, []string{"repo"})
	if err != nil {
		t.Errorf("a polled resource on a borrowed machine was refused: %v", err)
	}
}

func awaitFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err == nil {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("%s never appeared", path)
}
