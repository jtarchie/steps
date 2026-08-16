package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestConformancePinnedRunConsumesNothing: naming a version is an
// instruction outside the every-flow, so a pinned run neither reads the
// consumed cursor (Cache.unconsumed, long-standing) nor writes it.
//
// The write half became load-bearing when the cursor became a high-water
// mark over discovery order: a pinned version sitting ABOVE the mark would,
// if taken, leap the mark over every unbuilt version below it. Here the pin
// names v4 while v4 and v5 are still owed — a regression that takes it
// jumps the mark to 4 and silently cancels nothing less than the normal
// run's own v4 rebuild.
//
// Concourse's parallel: building a pinned version never advances
// NextEveryVersion's cursor, because discovery order is never consumed by an
// instruction.
func TestConformancePinnedRunConsumesNothing(t *testing.T) {
	dir := t.TempDir()
	posted := filepath.Join(dir, "posted.txt")
	feed := filepath.Join(dir, "feed.json")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: listing
  config:
    check: cat %s
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: listing
  source: {}
jobs:
- name: build
  plan:
  - get: items
    version: every
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, feed, posted)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	// The check reports v1..v3; the job builds them all (mark = 3).
	writeFixture(t, feed, `[{"n":"v1"},{"n":"v2"},{"n":"v3"}]`)

	err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	assertConformanceLineCount(t, posted, 3)

	// v4 and v5 arrive. An operator pins v4 — owed work, above the mark.
	writeFixture(t, feed, `[{"n":"v1"},{"n":"v2"},{"n":"v3"},{"n":"v4"},{"n":"v5"}]`)

	err = RunJob(ctx, cfg, &cfg.Jobs[0], map[string]string{"n": "v4"}, provider, st, false)
	if err != nil {
		t.Fatalf("pinned run: %v", err)
	}
	assertConformanceLineCount(t, posted, 4)

	// The normal run still owes BOTH v4 and v5 — the pin was an instruction,
	// not consumption. The task is changed first so the pinned run's chain
	// cannot satisfy v4 through the merkle cache: with an identical task the
	// cache would rightly skip v4 (work done is work done, and the skip path
	// takes it), which is correct but indistinguishable from a regression
	// that consumed v4 during the pinned run. A changed task forces a real
	// rebuild, so only the correct cursor state produces both versions.
	writeFixture(t, path, strings.Replace(pipelineYAML,
		"run: cat items/n.txt >> ", "run: cat items/n.txt | tee -a ", 1))

	cfg, err = config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("final run: %v", err)
	}
	assertConformanceLineCount(t, posted, 6)
}

// TestExplainResolvesFromHistoryLikeARun: `steps plan` must describe the run
// that would happen, which means resolving versions the same way — from
// recorded history, through the same Cache seam. Before this, Explain ran the
// live check while the run read history; for a cursor-driven check the live
// call has no cursor and answers with nothing, so `steps plan` errored with
// "no versions available" against a pipeline whose run worked fine.
func writeFixture(t *testing.T, path, contents string) {
	t.Helper()

	err := os.WriteFile(path, []byte(contents), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExplainResolvesFromHistoryLikeARun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	// A cursor-driven check: with no cursor it reports nothing, which is the
	// answer Explain used to be handed.
	pipelineYAML := `
resource_types:
- name: feed
  config:
    check: |
      printf '[]'
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
  - task: work
    inputs: [items]
    run: cat items/n.txt
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()

	// What a watch poll would have recorded.
	_, err = st.RecordVersions(ctx, "items", []map[string]any{{"n": "v1"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	rows, err := Explain(ctx, cfg, &cfg.Jobs[0], nil, st)
	if err != nil {
		t.Fatalf("Explain: %v — plan resolved differently than the run it describes", err)
	}

	for _, row := range rows {
		if !row.WouldSkip {
			t.Errorf("step %q would run (%s), want everything cached after an identical run", row.Name, row.Reason)
		}
	}
}
