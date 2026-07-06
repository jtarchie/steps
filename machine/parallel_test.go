package machine

import (
	"strings"
	"testing"
)

// diamondSrc is the canonical fork/join topology: fetch fans out to three
// heterogeneous analyses that run concurrently, then joins at merge. Each
// branch reads a PRE-FORK output ({ fetch }) — which only type-checks because
// the edges() reachability seed makes the fork's predecessors visible to a
// branch state.
const diamondSrc = `
const fetch = { prompt: "get the code", output: { code: "string" } };
const sec   = { prompt: ({ fetch }) => "audit " + fetch.code, output: { severity: "string" } };
const perf  = { prompt: ({ fetch }) => "perf "  + fetch.code, output: { hotspots: "string" } };
const style = { prompt: ({ fetch }) => "style " + fetch.code, output: { nits: "string" } };
const analysis = {
  parallel: { security: sec, perf: perf, style: style },
  concurrency: 3,
};
const merge = { prompt: ({ analysis }) => "report " + analysis.security.severity, output: { report: "string" } };
export default {
  name: "diamond",
  model: "mock",
  states: { fetch, sec, perf, style, analysis, merge },
  flow: pipe(fetch, analysis, merge, done),
};`

func TestParallelDiamondLoads(t *testing.T) {
	m, err := Parse([]byte(diamondSrc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fork := m.State("analysis")
	if fork.HandlerKind() != "parallel" {
		t.Fatalf("analysis handler kind = %q, want parallel", fork.HandlerKind())
	}
	if fork.Parallel == nil {
		t.Fatal("analysis has no ParallelSpec")
	}
	if got := len(fork.Parallel.Branches); got != 3 {
		t.Fatalf("branches = %d, want 3", got)
	}
	want := map[string]string{"security": "sec", "perf": "perf", "style": "style"}
	for _, b := range fork.Parallel.Branches {
		if want[b.Label] != b.Entry {
			t.Errorf("branch %q entry = %q, want %q", b.Label, b.Entry, want[b.Label])
		}
	}
	if fork.Parallel.Concurrency != 3 {
		t.Errorf("concurrency = %d, want 3", fork.Parallel.Concurrency)
	}
	if fork.Parallel.OnBranchFailure != "fail" {
		t.Errorf("onBranchFailure = %q, want fail (default)", fork.Parallel.OnBranchFailure)
	}

	// The fork's single out-edge is the join.
	if len(fork.Transitions) != 1 || fork.Transitions[0].To != "merge" {
		t.Errorf("fork transitions = %+v, want single fallback to merge", fork.Transitions)
	}
	// Each branch terminates at the shared done.
	for _, name := range []string{"sec", "perf", "style"} {
		st := m.State(name)
		if len(st.Transitions) != 1 || st.Transitions[0].To != "done" {
			t.Errorf("branch %q transitions = %+v, want single fallback to done", name, st.Transitions)
		}
	}
	// Reachability seed: branch states are reachable from initial.
	reach := reachableFrom(m, m.Initial)
	for _, name := range []string{"sec", "perf", "style"} {
		if !reach[name] {
			t.Errorf("branch state %q unreachable from initial — edges() seed missing", name)
		}
	}
}

func TestParallelGraphForkEdges(t *testing.T) {
	m, err := Parse([]byte(diamondSrc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := m.Graph()

	has := func(from, to string, kind EdgeKind) bool {
		for _, e := range g.Edges {
			if e.From == from && e.To == to && e.Kind == kind {
				return true
			}
		}
		return false
	}
	// The fork fans out to each branch entry...
	for _, entry := range []string{"sec", "perf", "style"} {
		if !has("analysis", entry, EdgeFork) {
			t.Errorf("missing fork edge analysis→%s", entry)
		}
	}
	// ...and its normal transition carries the join continuation.
	if !has("analysis", "merge", EdgeFallback) {
		t.Errorf("missing join edge analysis→merge")
	}
	// The fork node advertises its kind for the diagram.
	for _, n := range g.Nodes {
		if n.Name == "analysis" && n.Kind != "parallel" {
			t.Errorf("analysis node kind = %q, want parallel", n.Kind)
		}
	}
}

func TestParallelDefaults(t *testing.T) {
	src := strings.Replace(diamondSrc, "  concurrency: 3,\n", "", 1)
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := m.State("analysis").Parallel
	if p.Concurrency != 1 {
		t.Errorf("concurrency = %d, want 1 (default)", p.Concurrency)
	}
	if p.OnBranchFailure != "fail" {
		t.Errorf("onBranchFailure = %q, want fail (default)", p.OnBranchFailure)
	}
}

func TestParallelRequiresFlow(t *testing.T) {
	src := `
const sec  = { prompt: "audit" };
const perf = { prompt: "perf" };
const analysis = { parallel: { security: sec, perf: perf } };
export default { name: "no-flow", model: "mock", states: { sec, perf, analysis } };`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "needs a flow") {
		t.Fatalf("want a 'needs a flow' error, got %v", err)
	}
}

func TestParallelRequiresTwoBranches(t *testing.T) {
	src := `
const sec = { prompt: "audit" };
const analysis = { parallel: { security: sec } };
const merge = { prompt: "merge" };
export default {
  name: "one-branch", model: "mock",
  states: { sec, analysis, merge },
  flow: pipe(analysis, merge, done),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "at least two branches") {
		t.Fatalf("want an 'at least two branches' error, got %v", err)
	}
}

func TestParallelRejectsHumanInBranch(t *testing.T) {
	src := `
const gate = { human: "approve?" };
const perf = { prompt: "perf" };
const analysis = { parallel: { approval: gate, perf: perf } };
const merge = { prompt: "merge" };
export default {
  name: "gate-in-branch", model: "mock",
  states: { gate, perf, analysis, merge },
  flow: pipe(analysis, merge, done),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "cannot park") {
		t.Fatalf("want a 'cannot park' error for a gate in a branch, got %v", err)
	}
}

func TestParallelRejectsCrossForkAdopt(t *testing.T) {
	src := `
const fetch = { prompt: "get", output: { code: "string" } };
const sec   = { adopt: "fetch", prompt: "audit", output: { severity: "string" } };
const perf  = { prompt: "perf" };
const analysis = { parallel: { security: sec, perf: perf } };
const merge = { prompt: "merge" };
export default {
  name: "cross-adopt", model: "mock",
  states: { fetch, sec, perf, analysis, merge },
  flow: pipe(fetch, analysis, merge, done),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "across the fork boundary") {
		t.Fatalf("want an 'across the fork boundary' adopt error, got %v", err)
	}
}
