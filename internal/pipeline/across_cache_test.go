package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// countingCellCache counts the cell-cache reads a matrix makes.
type countingCellCache struct {
	store.Store

	single, batch atomic.Int32
}

func (c *countingCellCache) HasNodeSucceeded(ctx context.Context, jobName, hash string) (bool, error) {
	c.single.Add(1)

	return c.Store.HasNodeSucceeded(ctx, jobName, hash) //nolint:wrapcheck // a test passthrough
}

func (c *countingCellCache) HasNodesSucceeded(ctx context.Context, jobName string, hashes []string) (map[string]bool, error) {
	c.batch.Add(1)

	return c.Store.HasNodesSucceeded(ctx, jobName, hashes) //nolint:wrapcheck // a test passthrough
}

// TestAMatrixAsksTheCellCacheOnce: a matrix of N cacheable cells reads which
// of them already succeeded in one query, not N — and still skips exactly the
// cells that did, serially and under max_in_flight alike.
func TestAMatrixAsksTheCellCacheOnce(t *testing.T) {
	t.Parallel()

	m := newMatrixFixture(t)

	if _, ran := m.run(t, `"a", "b", "c"`, ""); ran != 3 {
		t.Fatalf("the first run executed %d cells, want all 3", ran)
	}

	// In order: each run adds the one cell the previous lacked.
	for _, matrix := range [][2]string{
		{`"a", "b", "c", "d"`, ""},
		{`"a", "b", "c", "d", "e"`, "\n    max_in_flight: 2"},
	} {
		inFlight := matrix[1]
		st, ran := m.run(t, matrix[0], inFlight)

		if got := st.batch.Load(); got != 1 {
			t.Errorf("max_in_flight %q: the matrix read the cell cache in %d batches, want 1", inFlight, got)
		}

		if got := st.single.Load(); got != 0 {
			t.Errorf("max_in_flight %q: the matrix asked about %d cells one at a time, want 0", inFlight, got)
		}

		if ran != 1 {
			t.Errorf("max_in_flight %q: executed %d cells, want only the new one", inFlight, ran)
		}
	}
}

// matrixFixture is one state file a matrix job reruns against.
type matrixFixture struct {
	dir      string
	inner    store.Store
	provider workspace.Provider
}

func newMatrixFixture(t *testing.T) matrixFixture {
	t.Helper()

	dir := t.TempDir()

	inner, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })

	return matrixFixture{dir: dir, inner: inner, provider: provider}
}

// run builds the matrix over values and returns the store's counts and how
// many cells executed.
func (m matrixFixture) run(t *testing.T, values, inFlight string) (*countingCellCache, int) {
	t.Helper()

	path := filepath.Join(m.dir, "fixture.yml")

	err := os.WriteFile(path, []byte(`
jobs:
- name: build
  plan:
  - across:
    - var: shard
      values: [`+values+`]`+inFlight+`
    task: unit
    run: "true"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st := &countingCellCache{Store: m.inner}

	var started atomic.Int32

	bus := events.New(func(e events.Event) {
		if e.Type == events.TypeStepStarted && e.StepKind == "task" {
			started.Add(1)
		}
	})

	err = RunJob(events.WithBus(context.Background(), bus), cfg, &cfg.Jobs[0], nil, m.provider, st, false)

	bus.Close()

	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	return st, int(started.Load())
}
