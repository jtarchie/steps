package pipeline

// resolveInputSets: the versions one run's builds bind, Concourse's way.

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
)

// setsFixture: a pipeline with gets over resources a and b whose checks read
// files, so tests control exactly what exists.
type setsFixture struct {
	cfg    *config.Config
	st     *store.Store
	cursor *versionCursor
}

// newSetsFixture builds the Config directly rather than via LoadConfig:
// these units test the resolver underneath the load rules, with fixtures a
// step more minimal than a loadable pipeline.
func newSetsFixture(t *testing.T, gets ...config.Step) *setsFixture {
	t.Helper()

	plan := append([]config.Step{}, gets...)
	plan = append(plan, config.Step{Task: "work", Run: "true", Inputs: config.Inputs()})

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name: "nothing",
			// The check matters only to a PINNED resolution, which runs it
			// live on purpose; everything else here reads recorded history.
			Config: config.ResourceTypeConfig{Check: `printf '[{"n":"1"},{"n":"2"},{"n":"3"}]'`, In: "true"},
		}, {
			Name:   "barren",
			Config: config.ResourceTypeConfig{Check: `printf '[]'`, In: "true"},
		}},
		Resources: []config.Resource{
			{Name: "a", Type: "nothing"},
			{Name: "b", Type: "nothing"},
		},
		Jobs: []config.Job{{Name: "build", Plan: plan}},
	}

	st, err := store.OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &setsFixture{cfg: cfg, st: st}
}

func everyGet(name string) config.Step  { return config.Step{Get: name, Version: "every"} }
func latestGet(name string) config.Step { return config.Step{Get: name} }

// record files versions 1..n for a resource, as a check would.
func (f *setsFixture) record(t *testing.T, resource string, n int) {
	t.Helper()

	versions := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		versions = append(versions, map[string]any{"n": strconv.Itoa(i)})
	}

	_, err := f.st.RecordVersions(context.Background(), resource, versions, 0)
	if err != nil {
		t.Fatal(err)
	}
}

// mark advances a job's cursor over a resource to order.
func (f *setsFixture) mark(t *testing.T, resource string, order int64) {
	t.Helper()

	err := f.st.RecordConsumedMark(context.Background(), "build", resource, order)
	if err != nil {
		t.Fatal(err)
	}
}

// resolve loads cursor+history fresh and computes the sets, the way a run
// would.
func (f *setsFixture) resolve(t *testing.T, pinned map[string]string) setResolution {
	t.Helper()

	ctx := context.Background()
	job := &f.cfg.Jobs[0]

	cursor, err := loadVersionCursor(ctx, f.st, job, true)
	if err != nil {
		t.Fatal(err)
	}

	history, err := loadResourceHistory(ctx, f.st, job)
	if err != nil {
		t.Fatal(err)
	}

	f.cursor = cursor
	cache := rsrc.NewCache(rsrc.WithConsumed(cursor.has), rsrc.WithResolvedVersions(history.get))

	resolution, err := resolveInputSets(ctx, f.cfg, job.Plan, pinned, cache, cursor, history)
	if err != nil {
		t.Fatal(err)
	}

	return resolution
}

// bindings renders sets compactly for assertion: "a1+b1 a2+b2".
func bindings(sets []merkle.InputSet) string {
	parts := make([]string, 0, len(sets))

	for _, set := range sets {
		parts = append(parts, fmt.Sprintf("a%v+b%v", set["a"]["n"], set["b"]["n"]))
	}

	return strings.Join(parts, " ")
}

// TestInputSetsLockstepDiagonal: several inputs with backlogs at once — cold
// recovery, a burst — advance one step each per set, and the shorter input
// holds at its last version while the longer finishes.
func TestInputSetsLockstepDiagonal(t *testing.T) {
	fixture := newSetsFixture(t, everyGet("a"), everyGet("b"))
	fixture.record(t, "a", 3)
	fixture.record(t, "b", 2)

	resolution := fixture.resolve(t, nil)

	if got := bindings(resolution.sets); got != "a1+b1 a2+b2 a3+b2" {
		t.Errorf("sets = %s, want a1+b1 a2+b2 a3+b2", got)
	}
}

// TestInputSetsStreamingInterleave is the common case: one resource moves at
// a time. Each round produces exactly one set pairing the new version with
// the sibling's held one.
func TestInputSetsStreamingInterleave(t *testing.T) {
	fixture := newSetsFixture(t, everyGet("a"), everyGet("b"))

	// Round 1: a1 and b1 exist, nothing consumed.
	fixture.record(t, "a", 1)
	fixture.record(t, "b", 1)

	if got := bindings(fixture.resolve(t, nil).sets); got != "a1+b1" {
		t.Fatalf("round 1 = %s, want a1+b1", got)
	}

	// The build ran: both cursors advance.
	fixture.mark(t, "a", 1)
	fixture.mark(t, "b", 1)

	// Round 2: b2 arrives alone.
	fixture.record(t, "b", 2)

	if got := bindings(fixture.resolve(t, nil).sets); got != "a1+b2" {
		t.Fatalf("round 2 = %s, want a1+b2 — a holds at its mark", got)
	}

	fixture.mark(t, "b", 2)

	// Round 3: a2 arrives alone.
	fixture.record(t, "a", 2)

	if got := bindings(fixture.resolve(t, nil).sets); got != "a2+b2" {
		t.Fatalf("round 3 = %s, want a2+b2 — b holds at its mark", got)
	}

	fixture.mark(t, "a", 2)

	// Round 4: nothing new anywhere — idle, not a build of held versions.
	if sets := fixture.resolve(t, nil).sets; len(sets) != 0 {
		t.Errorf("round 4 = %v, want no sets — nothing unconsumed means nothing to do", sets)
	}
}

// TestInputSetsHoldFallsBackBelowAPrunedMark: the hold is "newest candidate
// the cursor has covered", not "the version exactly at the mark" — the
// marked version may have been pruned from history, and <= is what degrades
// gracefully.
func TestInputSetsHoldFallsBackBelowAPrunedMark(t *testing.T) {
	fixture := newSetsFixture(t, everyGet("a"), everyGet("b"))
	fixture.record(t, "a", 2)
	fixture.record(t, "b", 3)

	// a's cursor sits at 5 — versions 3..5 were built and later pruned.
	fixture.mark(t, "a", 5)
	fixture.mark(t, "b", 2)

	resolution := fixture.resolve(t, nil)

	if got := bindings(resolution.sets); got != "a2+b3" {
		t.Errorf("sets = %s, want a2+b3 — a holds at the newest SURVIVING version its mark covers", got)
	}
}

// TestInputSetsUnsatisfiableInputBlocks: an every-get with no versions at
// all — nothing unconsumed, nothing ever taken to hold at — cannot bind, so
// no sets are built and the resource is named, even though its sibling has a
// backlog.
func TestInputSetsUnsatisfiableInputBlocks(t *testing.T) {
	fixture := newSetsFixture(t, everyGet("a"), everyGet("b"))
	fixture.record(t, "a", 3)
	// b: nothing, ever — no history, and its own check reports nothing, so
	// even the live-check fallback finds no version to bind.
	fixture.cfg.Resources[1].Type = "barren"

	resolution := fixture.resolve(t, nil)

	if len(resolution.sets) != 0 {
		t.Errorf("sets = %v, want none — b can bind nothing", resolution.sets)
	}

	if resolution.blockingReport() != "b" {
		t.Errorf("blocking = %q, want b named", resolution.blockingReport())
	}
}

// TestInputSetsNonEveryDrivenByEverySibling: a latest-mode get binds its one
// version into every set; the every-get's backlog decides how many sets
// there are.
func TestInputSetsNonEveryDrivenByEverySibling(t *testing.T) {
	fixture := newSetsFixture(t, latestGet("a"), everyGet("b"))
	fixture.record(t, "a", 2)
	fixture.record(t, "b", 3)

	resolution := fixture.resolve(t, nil)

	if got := bindings(resolution.sets); got != "a2+b1 a2+b2 a2+b3" {
		t.Errorf("sets = %s, want latest a in every set, b advancing", got)
	}
}

// TestInputSetsPinCollapsesToOneSet: a pin is an instruction. One set, the
// named versions, and (by the existing take exemption downstream) nothing
// consumed.
func TestInputSetsPinCollapsesToOneSet(t *testing.T) {
	fixture := newSetsFixture(t, everyGet("a"), everyGet("b"))
	fixture.record(t, "a", 3)
	fixture.record(t, "b", 3)

	resolution := fixture.resolve(t, map[string]string{"n": "2"})

	if got := bindings(resolution.sets); got != "a2+b2" {
		t.Errorf("sets = %s, want the pinned versions once", got)
	}

	if len(resolution.everyInputs) != 0 {
		t.Errorf("everyInputs = %v, want none — a pinned run consumes nothing", resolution.everyInputs)
	}
}

// TestInputSetsZeroEveryIsOneSet: a plan with no every-get is one build of
// the latest versions — today's shape, unchanged.
func TestInputSetsZeroEveryIsOneSet(t *testing.T) {
	fixture := newSetsFixture(t, latestGet("a"), latestGet("b"))
	fixture.record(t, "a", 2)
	fixture.record(t, "b", 1)

	resolution := fixture.resolve(t, nil)

	if got := bindings(resolution.sets); got != "a2+b1" {
		t.Errorf("sets = %s, want one set of latest versions", got)
	}
}
