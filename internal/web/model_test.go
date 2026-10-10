package web

import (
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestSpendSaysHowMuchOfTheBillItCovers: a total over some of the steps must say how many it left out, and a total over all of them must not pretend to have left any out.
func TestSpendSaysHowMuchOfTheBillItCovers(t *testing.T) {
	t.Parallel()

	price := 0.42

	mixed := runView{Usage: []store.AgentUsage{{Total: 10, CostUSD: &price}, {Total: 30}, {Total: 0}}}.Spend()
	if got := mixed.Cost(); got != "$0.4200+2?" {
		t.Errorf("mixed Cost() = %q, want the two unpriced steps counted", got)
	}

	priced := runView{Usage: []store.AgentUsage{{Total: 10, CostUSD: &price}}}.Spend()
	if got := priced.Cost(); got != "$0.4200" {
		t.Errorf("priced Cost() = %q, want no unpriced marker", got)
	}

	if got := (spendSummary{}).CachePercent(); got != 0 {
		t.Errorf("CachePercent with no tokens = %d, want 0", got)
	}

	if got := (usageView{}).CachePercent(); got != 0 {
		t.Errorf("a step's CachePercent with no tokens = %d, want 0", got)
	}

	if (runView{}).HasSpend() {
		t.Error("a run that never called a model claims spend")
	}
}

// TestDiffSkipsOnlyWhatItCannotCompare: a cache hit replays a node, so its hash is still compared; a skip with no hash ran nothing and is neither new nor changed.
func TestDiffSkipsOnlyWhatItCannotCompare(t *testing.T) {
	t.Parallel()

	prior := buildRunView(store.RunRow{ID: "prior"}, []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1, Status: "succeeded", Hash: "aaaa"},
	}, nil)

	current := buildRunView(store.RunRow{ID: "current"}, []store.RunEventRow{
		{Type: events.TypeStepSkipped, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1, Status: "skipped", Hash: "bbbb"},
		{Type: events.TypeStepSkipped, StepIndex: 1, StepName: "gate", StepKind: "task", StepID: 2, Status: "skipped"},
	}, nil)

	changed, _ := diffAgainst(current, prior)

	if changed["repo"] != "changed" {
		t.Errorf("a replayed step whose hash moved is not marked changed: %v", changed)
	}

	if _, marked := changed["gate"]; marked {
		t.Errorf("a hashless skip is marked %q: %v", changed["gate"], changed)
	}
}

// TestAHashlessStepDoesNotClaimASiblingsSpend: two members share an index and a name, and the row recorded under one's node belongs to that member alone.
func TestAHashlessStepDoesNotClaimASiblingsSpend(t *testing.T) {
	t.Parallel()

	view := buildRunView(store.RunRow{ID: "run"}, []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 2},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: "hash-for"},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 2, Status: "failed"},
	}, nil)
	view.Usage = []store.AgentUsage{{StepIndex: 0, StepName: "reviewer", NodeHash: "hash-for", Total: 10}}

	for _, step := range view.Steps {
		want := 0
		if step.ID == 1 {
			want = 1
		}

		if got := len(view.StepUsage(step)); got != want {
			t.Errorf("step %d draws %d spend rows, want %d", step.ID, got, want)
		}
	}
}
