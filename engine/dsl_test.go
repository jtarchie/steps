package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
)

// loadExampleFile loads a named machine file from an example directory,
// alongside its mock_responses.yaml.
func loadExampleFile(t *testing.T, dir, file string) (*machine.Machine, string) {
	t.Helper()
	wf := repoPath(t, filepath.Join("examples", dir, file))
	m, err := machine.Load(wf)
	if err != nil {
		t.Fatalf("load %s: %v", wf, err)
	}
	return m, repoPath(t, filepath.Join("examples", dir, "mock_responses.yaml"))
}

// TestSummarizeCriticDSLParity: the higher-level-DSL twin (verdict: + gate())
// lowers to the same graph and runs against the SAME mock, producing the exact
// trace of the object-form sibling (TestSummarizeCriticMockTrace).
func TestSummarizeCriticDSLParity(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExampleFile(t, "summarize-critic", "workflow-dsl.ts")
	eng, store := newTestEngine(t, script)
	rec := &recorder{}
	eng.Listener = rec

	res, err := eng.Start(context.Background(), m, map[string]any{"article": article(t)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone || res.Terminal != "done" {
		t.Fatalf("status = %s at %s, want done at done", res.Status, res.Terminal)
	}
	if res.State.Transitions != 5 {
		t.Errorf("transitions = %d, want 5 (parity with the object form)", res.State.Transitions)
	}
	if v := res.State.Visits["draft"]; v != 2 {
		t.Errorf("visits.draft = %d, want 2", v)
	}

	states, failed, _ := eventTrace(t, store, res.RunID)
	wantStates := []string{"draft", "critique", "draft", "critique", "publish"}
	if strings.Join(states, ",") != strings.Join(wantStates, ",") {
		t.Errorf("state sequence = %v, want %v", states, wantStates)
	}
	if failed["rate_limited"] != 1 || failed["schema_violation"] != 1 {
		t.Errorf("failures = %v, want one rate_limited + one schema_violation", failed)
	}

	// The synthesized gate exists but is never entered on this trace.
	if m.State("gate#critique_escalate") == nil {
		t.Error("gate#critique_escalate not synthesized")
	}
	if contains(states, "gate#critique_escalate") {
		t.Error("gate#critique_escalate should not be entered when the critic accepts")
	}

	checkEvidenceComposition(t, rec)
}

// checkEvidenceComposition asserts the evidence: blocks observed at the model
// boundary: visit one carries the ARTICLE block and no feedback; the revisit
// carries the reviewer's issues under the derived REVIEWER FEEDBACK header.
func checkEvidenceComposition(t *testing.T, rec *recorder) {
	t.Helper()
	drafts := rec.user["draft"]
	if len(drafts) != 2 {
		t.Fatalf("draft prompts = %d, want 2", len(drafts))
	}
	if !strings.Contains(drafts[0], "ARTICLE:\n") || strings.Contains(drafts[0], "REVIEWER FEEDBACK:") {
		t.Errorf("first draft prompt = %.120q, want ARTICLE block and no feedback", drafts[0])
	}
	if !strings.Contains(drafts[1], "REVIEWER FEEDBACK:\n") || !strings.Contains(drafts[1], "Ideal X") {
		t.Errorf("revisit prompt = %.120q, want the REVIEWER FEEDBACK block with the critic's issues", drafts[1])
	}
}

// TestCodegenDSLParity: the all-four-features twin (tiers + verdict + carry +
// gate) runs against the same mock and produces the exact trace of
// TestCodegenMockTrace — including the real build gate executing for real.
func TestCodegenDSLParity(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExampleFile(t, "codegen", "workflow-dsl.ts")
	eng, store := newTestEngine(t, script)
	spec, err := os.ReadFile(repoPath(t, "examples/codegen/fixtures/spec.md"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := eng.Start(context.Background(), m, map[string]any{
		"spec":       string(spec),
		"language":   "bash",
		"out":        "out",
		"verify_cmd": "bash greet_test.sh",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	states, _, transitions := eventTrace(t, store, res.RunID)
	want := []string{
		"plan",
		"generate#spec", "generate#build_cause", "generate", "review",
		"generate#spec", "generate#build_cause", "generate", "review",
		"write_files", "build", "report",
	}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	if transitions != 12 {
		t.Errorf("journaled transitions = %d, want 12", transitions)
	}
	if res.State.Transitions != 8 {
		t.Errorf("counted transitions = %d, want 8", res.State.Transitions)
	}

	// carry: the coder aggregate pairs each output with its planned file.
	gen, _ := res.State.Ctx["generate"].(map[string]any)
	items, _ := gen["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("generate.items = %d, want 2 carried entries", len(items))
	}
	first, _ := items[0].(map[string]any)
	if _, ok := first["item"]; !ok {
		t.Errorf("carried entry = %v, want an {item, output, index} shape", first)
	}
	if idx, _ := first["index"].(int); idx != 0 {
		t.Errorf("first carried index = %v, want 0", first["index"])
	}

	// The two human tie-breaks were synthesized but never entered.
	for _, g := range []string{"gate#escalate", "gate#accept_build"} {
		if m.State(g) == nil {
			t.Errorf("%s not synthesized", g)
		}
		if contains(states, g) {
			t.Errorf("%s should not be entered on the happy path", g)
		}
	}
}

// TestCodegenBuilderParity: the builder-closure twin (state(name, s => ...))
// lowers to the same graph and runs against the SAME mock, producing the exact
// trace of the object form (TestCodegenMockTrace) — proving state(build) is
// pure sugar over the literal.
func TestCodegenBuilderParity(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExampleFile(t, "codegen", "workflow-builder.ts")
	eng, store := newTestEngine(t, script)
	spec, err := os.ReadFile(repoPath(t, "examples/codegen/fixtures/spec.md"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := eng.Start(context.Background(), m, map[string]any{
		"spec":       string(spec),
		"language":   "bash",
		"out":        "out",
		"verify_cmd": "bash greet_test.sh",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	states, _, transitions := eventTrace(t, store, res.RunID)
	want := []string{
		"plan",
		"generate#spec", "generate#build_cause", "generate", "review",
		"generate#spec", "generate#build_cause", "generate", "review",
		"write_files", "build", "report",
	}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	if transitions != 12 {
		t.Errorf("journaled transitions = %d, want 12", transitions)
	}
	if res.State.Transitions != 8 {
		t.Errorf("counted transitions = %d, want 8", res.State.Transitions)
	}

	// The build gate really executed the generated test (exit code is data).
	gen, _ := res.State.Ctx["generate"].(map[string]any)
	if n, _ := gen["count"].(int); n != 2 {
		t.Errorf("generate.count = %v, want 2 (one hermetic context per file)", gen["count"])
	}

	// Both human tie-breaks exist as normal (non-synthesized) states, never entered.
	for _, g := range []string{"escalate", "accept_build"} {
		if m.State(g) == nil {
			t.Errorf("%s missing", g)
		}
		if contains(states, g) {
			t.Errorf("%s should not be entered on the happy path", g)
		}
	}
}

// TestPRReviewDSLParity: the model-tier twin runs against the same mock and
// produces the exact deep-path trace of TestPRReviewDeepPath.
func TestPRReviewDSLParity(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExampleFile(t, "pr-review", "workflow-dsl.ts")
	eng, store := newTestEngine(t, script)
	diff, err := os.ReadFile(repoPath(t, "examples/pr-review/fixtures/pr.diff"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := eng.Start(context.Background(), m, map[string]any{
		"diff":        string(diff),
		"root":        repoPath(t, "examples/pr-review/fixtures/repo"),
		"title":       "queue: parallel worker pool",
		"description": "Process jobs concurrently",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	states, _, transitions := eventTrace(t, store, res.RunID)
	want := []string{"fetch_pr", "split_diff", "scout_files", "scout_pr", "deep_review", "verdict", "write_review"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	if transitions != 7 {
		t.Errorf("transitions = %d, want 7", transitions)
	}

	// The scout tier's memo lowered onto scout_files (no explicit memo: on it).
	if !m.State("scout_files").Memo {
		t.Error("scout_files.memo = false, want true from the scout tier")
	}
	if !m.State("verdict").Memo {
		t.Error("verdict.memo = false, want true from the senior tier")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
