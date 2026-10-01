package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestConcurrentRunsOfOneJobTakeDifferentVersions: two claims of one job
// under max_in_flight > 1 each build a version of their own. Each run waits at
// the seam for the other to arrive — so without the lock both resolve the
// oldest version before either takes it, and both build it.
func TestConcurrentRunsOfOneJobTakeDifferentVersions(t *testing.T) {
	dir := t.TempDir()
	built := filepath.Join(dir, "built.txt")
	path := filepath.Join(dir, "pipeline.yml")

	writeFixture(t, path, fmt.Sprintf(`
resource_types:
- name: listing
  config:
    check: printf '[{"n":"v1"},{"n":"v2"}]'
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: listing
  source: {}
jobs:
- name: build
  max_in_flight: 2
  plan:
  - get: items
    version: every
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, built))

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	var arrived sync.WaitGroup

	arrived.Add(2)

	resolvedSets = func() {
		arrived.Done()

		waited := make(chan struct{})

		go func() {
			arrived.Wait()
			close(waited)
		}()

		select {
		case <-waited:
		case <-time.After(500 * time.Millisecond):
		}
	}

	t.Cleanup(func() { resolvedSets = func() {} })

	var runs sync.WaitGroup

	for range 2 {
		runs.Go(func() {
			runErr := RunJob(context.Background(), cfg, &cfg.Jobs[0], nil, provider, st, false)
			if runErr != nil {
				t.Errorf("RunJob: %v", runErr)
			}
		})
	}

	runs.Wait()

	data, err := os.ReadFile(built) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Fields(string(data))
	slices.Sort(got)

	if !slices.Equal(got, []string{"v1", "v2"}) {
		t.Errorf("two concurrent runs built %v, want v1 and v2 once each", got)
	}
}
