package engine_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
	"github.com/jtarchie/steps/toolreg"
)

// diamondMachine: fetch fans out to three heterogeneous branches that run
// concurrently, then joins at merge which reads all three by label.
const diamondMachine = `
const fetch = { prompt: "get the code", output: { code: "string" } };
const sec   = { prompt: ({ fetch }) => "audit " + fetch.code, output: { severity: "string" } };
const perf  = { prompt: ({ fetch }) => "perf "  + fetch.code, output: { hotspots: "string" } };
const style = { prompt: ({ fetch }) => "style " + fetch.code, output: { nits: "string" } };
const analysis = {
  parallel: { security: sec, perf: perf, style: style },
  concurrency: 3,
};
const merge = {
  prompt: ({ analysis }) => "report: " + analysis.security.severity + " " + analysis.perf.hotspots + " " + analysis.style.nits,
  output: { report: "string" },
};
export default {
  name: "diamond",
  model: "mock",
  states: { fetch, sec, perf, style, analysis, merge },
  flow: pipe(fetch, analysis, merge, done),
};`

func diamondEngine(t *testing.T, script provider.Script) (*engine.Engine, *journal.SQLiteStore) {
	t.Helper()
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), nil)
	eng.Mock = script
	return eng, store
}

func TestParallelDiamondSerial(t *testing.T) {
	m, err := machine.Parse([]byte(diamondMachine))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	eng, store := diamondEngine(t, provider.Script{
		"fetch": {{Text: `{"code":"func main"}`}},
		"sec":   {{Text: `{"severity":"high"}`}},
		"perf":  {{Text: `{"hotspots":"n+1 query"}`}},
		"style": {{Text: `{"nits":"naming"}`}},
		"merge": {{Text: `{"report":"consolidated"}`}},
	})

	res, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone || res.Terminal != "done" {
		t.Fatalf("status = %s at %s, want done at done", res.Status, res.Terminal)
	}

	// The parent journal stays single-cursor: it never enters a branch state.
	// Its state sequence is fetch -> analysis (the fork) -> merge.
	states, _, _ := eventTrace(t, store, res.RunID)
	wantParent := []string{"fetch", "analysis", "merge"}
	if got := join(states); got != join(wantParent) {
		t.Errorf("parent state sequence = %v, want %v", states, wantParent)
	}
	for _, branch := range []string{"sec", "perf", "style"} {
		for _, s := range states {
			if s == branch {
				t.Errorf("parent journal entered branch state %q — branches must run in child runs", branch)
			}
		}
	}

	// The fork's aggregate is label-keyed and reaches the join.
	agg, ok := res.State.Ctx["analysis"].(map[string]any)
	if !ok {
		t.Fatalf("ctx.analysis = %T, want the label-keyed aggregate", res.State.Ctx["analysis"])
	}
	sec, ok := agg["security"].(map[string]any)
	if !ok || sec["severity"] != "high" {
		t.Errorf("analysis.security = %v, want {severity: high}", agg["security"])
	}
	if _, ok := agg["perf"].(map[string]any); !ok {
		t.Errorf("analysis.perf missing from aggregate: %v", agg)
	}
	if _, ok := agg["style"].(map[string]any); !ok {
		t.Errorf("analysis.style missing from aggregate: %v", agg)
	}

	// Usage summed across the three branch child runs plus fetch + merge.
	if res.State.Usage.Total() == 0 {
		t.Errorf("run usage = 0, want branch usage folded into the parent")
	}
}

// failForkMachine forks three branches where beta always fails (mock error, no
// retry). onBranchFailure is templated in.
const failForkMachine = `
const a = { prompt: "a", output: { v: "string" } };
const b = { prompt: "b", retry: "none", output: { v: "string" } };
const c = { prompt: "c", output: { v: "string" } };
const fork = { parallel: { alpha: a, beta: b, gamma: c }%s };
const merge = { prompt: "m", output: { r: "string" } };
export default {
  name: "failfork",
  model: "mock",
  states: { a, b, c, fork, merge },
  flow: pipe(fork, merge, done),
};`

func failForkScript() provider.Script {
	return provider.Script{
		"a":     {{Text: `{"v":"A"}`}},
		"b":     {{Error: "provider_error"}},
		"c":     {{Text: `{"v":"C"}`}},
		"merge": {{Text: `{"r":"done"}`}},
	}
}

func TestParallelFailPolicyFailsFork(t *testing.T) {
	m, err := machine.Parse([]byte(fmt.Sprintf(failForkMachine, "")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	eng, _ := diamondEngine(t, failForkScript())

	res, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusFailed {
		t.Fatalf("status = %s, want failed (a branch failed under onBranchFailure=fail)", res.Status)
	}
	if _, ok := res.State.Ctx["merge"]; ok {
		t.Errorf("merge ran despite the fork failing: %v", res.State.Ctx["merge"])
	}
}

func TestParallelCollectPolicyContinues(t *testing.T) {
	m, err := machine.Parse([]byte(fmt.Sprintf(failForkMachine, `, onBranchFailure: "collect"`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	eng, _ := diamondEngine(t, failForkScript())

	res, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s, want done (collect lets the fork continue)", res.Status)
	}
	agg, _ := res.State.Ctx["fork"].(map[string]any)
	if _, ok := agg["alpha"].(map[string]any); !ok {
		t.Errorf("aggregate missing surviving branch alpha: %v", agg)
	}
	if _, ok := agg["gamma"].(map[string]any); !ok {
		t.Errorf("aggregate missing surviving branch gamma: %v", agg)
	}
	fails, ok := agg["_failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("_failures = %v, want exactly one (beta)", agg["_failures"])
	}
	f0, _ := fails[0].(map[string]any)
	if f0["label"] != "beta" {
		t.Errorf("failed branch label = %v, want beta", f0["label"])
	}
}

// sleepActionEngine builds an engine (no mock, so concurrency is real) with a
// test.sleep action that honors context cancellation and a test.noop.
func sleepActionEngine(t *testing.T) *engine.Engine {
	t.Helper()
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	reg := toolreg.New()
	reg.Register("test.sleep", "sleep ms", func(ctx context.Context, args map[string]any) (map[string]any, error) {
		ms := 0
		switch v := args["ms"].(type) {
		case float64:
			ms = int(v)
		case int64:
			ms = int(v)
		case int:
			ms = v
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
			return map[string]any{"slept": ms}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	reg.Register("test.noop", "noop", func(ctx context.Context, args map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	return engine.New(store, provider.NewRegistry(), reg, nil)
}

func TestParallelConcurrencyIsParallel(t *testing.T) {
	src := `
const a = { action: "test.sleep", input: { ms: 250 } };
const b = { action: "test.sleep", input: { ms: 250 } };
const c = { action: "test.sleep", input: { ms: 250 } };
const fork = { parallel: { alpha: a, beta: b, gamma: c }, concurrency: 3 };
const merge = { action: "test.noop" };
export default {
  name: "concurrent",
  states: { a, b, c, fork, merge },
  flow: pipe(fork, merge, done),
};`
	m, err := machine.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	eng := sleepActionEngine(t)

	start := time.Now()
	res, err := eng.Start(context.Background(), m, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s, want done", res.Status)
	}
	// Three 250ms sleeps: serial would be ~750ms, parallel ~250ms.
	if elapsed > 600*time.Millisecond {
		t.Errorf("elapsed %v, want < 600ms — branches did not run concurrently", elapsed)
	}
}

func TestParallelDeadlineCancels(t *testing.T) {
	src := `
const a = { action: "test.sleep", input: { ms: 5000 }, retry: "none" };
const b = { action: "test.sleep", input: { ms: 5000 }, retry: "none" };
const fork = { parallel: { alpha: a, beta: b }, concurrency: 2 };
const merge = { action: "test.noop" };
export default {
  name: "deadline",
  limits: { timeout: "150ms" },
  states: { a, b, fork, merge },
  flow: pipe(fork, merge, done),
};`
	m, err := machine.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	eng := sleepActionEngine(t)

	start := time.Now()
	res, err := eng.Start(context.Background(), m, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusFailed {
		t.Fatalf("status = %s, want failed (branches exceed the fork deadline)", res.Status)
	}
	// The 5s sleeps must have been cancelled at the ~150ms deadline, not awaited.
	if elapsed > 2*time.Second {
		t.Errorf("elapsed %v — the fork deadline did not cancel hung branches", elapsed)
	}
}

// faultStore wraps a real store and "dies" (fails every subsequent write) once
// tripOn matches an appended event — a deterministic mid-fork crash.
type faultStore struct {
	journal.Store
	mu     sync.Mutex
	dead   bool
	tripOn func(*journal.Event) bool
}

func (f *faultStore) isDead() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dead
}

func (f *faultStore) Append(ctx context.Context, ev *journal.Event) (int, error) {
	if f.isDead() {
		return 0, errors.New("simulated crash: store is gone")
	}
	seq, err := f.Store.Append(ctx, ev)
	if err == nil && f.tripOn != nil && f.tripOn(ev) {
		f.mu.Lock()
		f.dead = true
		f.mu.Unlock()
	}
	return seq, err //nolint:wrapcheck
}

func (f *faultStore) CreateRun(ctx context.Context, run *journal.Run) error {
	if f.isDead() {
		return errors.New("simulated crash: store is gone")
	}
	return f.Store.CreateRun(ctx, run) //nolint:wrapcheck
}

func (f *faultStore) UpdateRun(ctx context.Context, id, status, current string) error {
	if f.isDead() {
		return errors.New("simulated crash: store is gone")
	}
	return f.Store.UpdateRun(ctx, id, status, current) //nolint:wrapcheck
}

// crashDrillMachine: a bare fork of three branches into a join — no pre-fork
// state, so Initial is the fork itself.
const crashDrillMachine = `
const a = { prompt: "branch a", output: { v: "string" } };
const b = { prompt: "branch b", output: { v: "string" } };
const c = { prompt: "branch c", output: { v: "string" } };
const fork = { parallel: { alpha: a, beta: b, gamma: c } };
const merge = { prompt: ({ fork }) => "m " + fork.alpha.v + fork.beta.v + fork.gamma.v, output: { r: "string" } };
export default {
  name: "crash-drill",
  model: "mock",
  states: { a, b, c, fork, merge },
  flow: pipe(fork, merge, done),
};`

// TestParallelCrashMidForkResume is the durability drill: the store dies right
// after branch beta is entered — alpha finished, beta mid-flight, gamma never
// created. Resume must reattach to the SAME pinned children (no respawn), not
// re-run the finished branch, and drive the run to completion.
func TestParallelCrashMidForkResume(t *testing.T) {
	m, err := machine.Parse([]byte(crashDrillMachine))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	script := provider.Script{
		"a":     {{Text: `{"v":"A"}`}},
		"b":     {{Text: `{"v":"B"}`}},
		"c":     {{Text: `{"v":"C"}`}},
		"merge": {{Text: `{"r":"done"}`}},
	}

	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// Phase 1: crash. The store dies after beta's state_entered.
	fault := &faultStore{Store: store, tripOn: func(ev *journal.Event) bool {
		return ev.Type == journal.StateEntered && ev.Data["state"] == "b"
	}}
	crashEng := engine.New(fault, provider.NewRegistry(), toolreg.New(), nil)
	crashEng.Mock = script

	res, err := crashEng.Start(context.Background(), m, nil)
	if err == nil {
		t.Fatalf("expected the simulated crash to abort Start, got status %s", res.Status)
	}

	// The parent run and its fork_started children survived the crash.
	parentID := onlyParentRun(t, store)
	pinned := forkChildIDs(t, store, parentID)
	if len(pinned) != 3 {
		t.Fatalf("fork_started pinned %d children, want 3", len(pinned))
	}

	// Phase 2: resume with a fresh engine on the healthy store.
	resumeEng := engine.New(store, provider.NewRegistry(), toolreg.New(), nil)
	resumeEng.Mock = script
	out, err := resumeEng.Resume(context.Background(), m, parentID, "", nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if out.Status != journal.StatusDone || out.Terminal != "done" {
		t.Fatalf("resumed status = %s at %s, want done at done", out.Status, out.Terminal)
	}

	// No respawn: exactly the three pinned children exist, no more.
	assertNoRespawn(t, store, parentID, pinned)

	// The finished branch (alpha → state a) was not re-run.
	if n := handlerFinishedCount(t, store, pinned[0], "a"); n != 1 {
		t.Errorf("branch alpha state a handler_finished ×%d, want 1 (not re-run on resume)", n)
	}

	// The join saw all three branches.
	agg, _ := out.State.Ctx["fork"].(map[string]any)
	for _, label := range []string{"alpha", "beta", "gamma"} {
		if _, ok := agg[label].(map[string]any); !ok {
			t.Errorf("aggregate missing branch %q: %v", label, agg)
		}
	}
}

// assertNoRespawn checks that exactly the pinned children survive resume —
// no additional children were spawned and none of the pinned set is missing.
func assertNoRespawn(t *testing.T, store journal.Store, parentID string, pinned []string) {
	t.Helper()
	children, err := store.ListChildRuns(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 3 {
		t.Errorf("child runs = %d, want exactly 3 (no respawn)", len(children))
	}
	got := map[string]bool{}
	for _, ch := range children {
		got[ch.ID] = true
	}
	for _, id := range pinned {
		if !got[id] {
			t.Errorf("pinned child %q missing after resume — a new set was spawned", id)
		}
	}
}

func onlyParentRun(t *testing.T, store journal.Store) string {
	t.Helper()
	runs, err := store.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var parent string
	for _, r := range runs {
		if r.ParentRunID == "" {
			if parent != "" {
				t.Fatalf("more than one top-level run: %s and %s", parent, r.ID)
			}
			parent = r.ID
		}
	}
	if parent == "" {
		t.Fatal("no top-level run found")
	}
	return parent
}

func forkChildIDs(t *testing.T, store journal.Store, runID string) []string {
	t.Helper()
	events, err := store.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != journal.ForkStarted {
			continue
		}
		var payload struct {
			Children []journal.ChildRef `json:"children"`
		}
		err := journal.DecodeData(ev, &payload)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(payload.Children))
		for i, c := range payload.Children {
			ids[i] = c.RunID
		}
		return ids
	}
	t.Fatal("no fork_started event in parent journal")
	return nil
}

func handlerFinishedCount(t *testing.T, store journal.Store, runID, state string) int {
	t.Helper()
	events, err := store.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type == journal.HandlerFinished && ev.Data["state"] == state {
			n++
		}
	}
	return n
}

func TestParallelChildrenHiddenFromRunsList(t *testing.T) {
	m, err := machine.Parse([]byte(diamondMachine))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	eng, store := diamondEngine(t, provider.Script{
		"fetch": {{Text: `{"code":"x"}`}},
		"sec":   {{Text: `{"severity":"high"}`}},
		"perf":  {{Text: `{"hotspots":"n"}`}},
		"style": {{Text: `{"nits":"m"}`}},
		"merge": {{Text: `{"report":"r"}`}},
	})
	res, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The top-level list shows only the parent; branches are hidden.
	runs, err := store.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != res.RunID {
		t.Errorf("ListRuns = %d runs, want just the parent %s", len(runs), res.RunID)
	}
	// But the three children are reachable under the parent.
	kids, err := store.ListChildRuns(context.Background(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 3 {
		t.Errorf("ListChildRuns = %d, want 3", len(kids))
	}
	for _, k := range kids {
		if k.ParentRunID != res.RunID {
			t.Errorf("child %s parent = %q, want %s", k.ID, k.ParentRunID, res.RunID)
		}
	}
}

func join(ss []string) string {
	out := ""
	var outSb529 strings.Builder
	for i, s := range ss {
		if i > 0 {
			outSb529.WriteString(",")
		}
		outSb529.WriteString(s)
	}
	out += outSb529.String()
	return out
}
