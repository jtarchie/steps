package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/workspace"
)

// gateFixture is a two-branch race gate, a context its notes reach, and a channel each "waits for its outcome" note lands on: the one moment a branch is known to be blocked, observed rather than slept for.
func gateFixture(t *testing.T) (*raceGate, context.Context, <-chan string, *bool) {
	t.Helper()

	cancelled := false
	gate := newRaceGate([]branchResult{{name: "fast"}, {name: "slow"}}, func() { cancelled = true })

	waiting := make(chan string, 4)
	bus := events.New(nil)
	stop := bus.Observe(func(e events.Event) {
		if e.Type == events.TypeStepNote && strings.Contains(e.Text, "waits for its outcome") {
			waiting <- e.Text
		}
	})

	t.Cleanup(func() { stop(); bus.Close() })

	return gate, events.WithBus(t.Context(), bus), waiting, &cancelled
}

// TestARaceLoserWaitsAndIsRefused: once a branch holds the race's outputs, another's write waits for its outcome, and is refused when it wins.
func TestARaceLoserWaitsAndIsRefused(t *testing.T) {
	t.Parallel()

	gate, ctx, waiting, cancelled := gateFixture(t)

	took, err := gate.claim(ctx, 0, true)
	if !took || err != nil {
		t.Fatalf("the first claim = %v, %v; want taken", took, err)
	}

	claimed := make(chan error, 1)

	go func() {
		_, err := gate.claim(ctx, 1, true)
		claimed <- err
	}()

	select {
	case <-waiting:
	case err := <-claimed:
		t.Fatalf("slow claimed outputs fast holds without waiting: %v", err)
	}

	gate.finish(ctx, 0, true)

	err = <-claimed
	if !errors.Is(err, errRaceLost) {
		t.Errorf("slow's claim after fast won = %v, want errRaceLost", err)
	}

	if gate.won() != 0 || !*cancelled {
		t.Errorf("winner = %d, cancelled = %v; want fast, and the losers cancelled", gate.won(), *cancelled)
	}
}

// TestARaceOwnersFailureHandsTheOutputsOn: the holder failing gives the outputs to the branch waiting on it, which can then win.
func TestARaceOwnersFailureHandsTheOutputsOn(t *testing.T) {
	t.Parallel()

	gate, ctx, waiting, _ := gateFixture(t)

	_, _ = gate.claim(ctx, 0, true)

	claimed := make(chan error, 1)

	go func() {
		_, err := gate.claim(ctx, 1, true)
		claimed <- err
	}()

	<-waiting
	gate.finish(ctx, 0, false)

	gate.mu.Lock()
	kept := gate.owner == 0
	gate.mu.Unlock()

	if kept {
		t.Fatal("fast still holds the outputs after failing; slow would wait forever")
	}

	err := <-claimed
	if err != nil {
		t.Fatalf("slow's claim after fast failed = %v, want the outputs", err)
	}

	gate.finish(ctx, 1, true)

	if gate.won() != 1 {
		t.Errorf("winner = %d, want slow", gate.won())
	}
}

// TestARaceSuccessThatWroteNothingWaitsForTheHolder: a branch that succeeded without writing does not win over one already holding the outputs, which would leave the holder's bytes as the winner's.
func TestARaceSuccessThatWroteNothingWaitsForTheHolder(t *testing.T) {
	t.Parallel()

	gate, ctx, waiting, _ := gateFixture(t)

	_, _ = gate.claim(ctx, 0, true)

	finished := make(chan struct{})

	go func() {
		gate.finish(ctx, 1, true)
		close(finished)
	}()

	select {
	case <-waiting:
	case <-finished:
		t.Fatal("slow won while fast held the outputs")
	}

	gate.finish(ctx, 0, true)
	<-finished

	if gate.won() != 0 {
		t.Errorf("winner = %d, want fast, whose outputs are the ones written", gate.won())
	}
}

// TestHoldArtifactsGivesBackOnlyWhatItTook: a cache lookup that restored nothing gives back a claim it took, never one its branch already held from a capture.
func TestHoldArtifactsGivesBackOnlyWhatItTook(t *testing.T) {
	t.Parallel()

	gate, ctx, waiting, _ := gateFixture(t)
	fast := withRaceSeat(ctx, gate, 0)

	_, err := holdArtifacts(fast, true)
	if err != nil {
		t.Fatal(err)
	}

	release, err := holdArtifacts(fast, true)
	if err != nil {
		t.Fatal(err)
	}

	release()

	claimed := make(chan error, 1)

	go func() {
		_, err := gate.claim(ctx, 1, true)
		claimed <- err
	}()

	select {
	case <-waiting:
	case err := <-claimed:
		t.Fatalf("a release gave away outputs fast captured: slow claimed them (%v)", err)
	}

	gate.finish(ctx, 0, true)
	<-claimed

	fresh := newRaceGate([]branchResult{{name: "a"}, {name: "b"}}, func() {})

	release, err = holdArtifacts(withRaceSeat(ctx, fresh, 1), true)
	if err != nil {
		t.Fatal(err)
	}

	release()

	if fresh.owner != -1 {
		t.Errorf("owner = %d after releasing a claim this call took, want none", fresh.owner)
	}
}

// restoreProbe is a build whose step cache answers hit or miss, noting which race branch held the outputs while it restored.
type restoreProbe struct {
	workspace.BuildWorkspace

	gate   *raceGate
	hit    bool
	heldBy int
}

func (p *restoreProbe) RestoreStep(context.Context, workspace.StepCacheRequest) (workspace.StepCacheResult, error) {
	p.gate.mu.Lock()
	p.heldBy = p.gate.owner
	p.gate.mu.Unlock()

	return workspace.StepCacheResult{Key: "k", Hit: p.hit}, nil
}

// TestARaceBranchRestoresFromTheCacheHoldingTheOutputs: a step cache hit writes the race's outputs as a capture does, so it holds them through the restore, and keeps them only when it restored something.
func TestARaceBranchRestoresFromTheCacheHoldingTheOutputs(t *testing.T) {
	t.Parallel()

	step := config.Step{Task: "summarize", Run: "true", Outputs: []string{"summary"}}

	for _, hit := range []bool{true, false} {
		gate := newRaceGate([]branchResult{{name: "fast"}, {name: "slow"}}, func() {})
		probe := &restoreProbe{gate: gate, hit: hit, heldBy: -2}

		res := lookupStepCache(withRaceSeat(t.Context(), gate, 1), stepRunner{cfg: &config.Config{}, bw: probe}, step, workspace.StepCacheRequest{}, "summarize")

		if res.Hit != hit || probe.heldBy != 1 {
			t.Fatalf("hit %v: restored %v holding %d, want the restore made by slow holding the outputs", hit, res.Hit, probe.heldBy)
		}

		if want := map[bool]int{true: 1, false: -1}[hit]; gate.owner != want {
			t.Errorf("hit %v: the outputs are held by %d after the lookup, want %d", hit, gate.owner, want)
		}
	}
}

func (p *restoreProbe) ResetArtifact(context.Context, string) error {
	p.gate.mu.Lock()
	p.heldBy = p.gate.owner
	p.gate.mu.Unlock()

	return nil
}

// TestARaceBranchSkipsTheCacheWhileASiblingHoldsTheOutputs: a branch starting while another holds the race's outputs runs rather than wait to look, and writes nothing.
func TestARaceBranchSkipsTheCacheWhileASiblingHoldsTheOutputs(t *testing.T) {
	t.Parallel()

	gate, ctx, waiting, _ := gateFixture(t)
	probe := &restoreProbe{gate: gate, hit: true, heldBy: -2}

	_, _ = gate.claim(ctx, 0, true)

	step := config.Step{Task: "summarize", Run: "true", Outputs: []string{"summary"}}
	looked := make(chan workspace.StepCacheResult, 1)

	go func() {
		looked <- lookupStepCache(withRaceSeat(ctx, gate, 1), stepRunner{cfg: &config.Config{}, bw: probe}, step, workspace.StepCacheRequest{}, "summarize")
	}()

	var res workspace.StepCacheResult

	select {
	case res = <-looked:
	case <-waiting:
		t.Error("slow waited on fast to look in the cache")
		gate.finish(ctx, 0, true)

		res = <-looked
	}

	if res.Hit || probe.heldBy != -2 {
		t.Errorf("slow restored %v from the cache (asked: %v) while fast held the outputs, want it to run", res.Hit, probe.heldBy != -2)
	}

	if gate.owner != 0 {
		t.Errorf("the outputs are held by %d, want fast still", gate.owner)
	}
}

// TestARaceBranchCollectsHoldingTheOutputs: a collecting matrix empties its artifact before any cell runs, which in a race: is a write like a capture.
func TestARaceBranchCollectsHoldingTheOutputs(t *testing.T) {
	t.Parallel()

	gate := newRaceGate([]branchResult{{name: "fast"}, {name: "slow"}}, func() {})
	probe := &restoreProbe{gate: gate, heldBy: -2}
	matrix := config.Step{Task: "scan", Run: "true", Outputs: []string{"findings"}, Across: []config.AcrossVar{{Var: "x", Values: []string{"a"}}}}

	err := resetCollectedArtifacts(withRaceSeat(t.Context(), gate, 1), "step 0", matrix, probe)
	if err != nil {
		t.Fatal(err)
	}

	if probe.heldBy != 1 {
		t.Errorf("the matrix emptied its artifact held by %d, want slow", probe.heldBy)
	}
}

// TestHoldArtifactsInANestedRaceTakesBothOrNeither: a step in a race inside a race writes for both; refused by the inner one, it gives the outer claim back.
func TestHoldArtifactsInANestedRaceTakesBothOrNeither(t *testing.T) {
	t.Parallel()

	outer := newRaceGate([]branchResult{{name: "a"}, {name: "b"}}, func() {})
	inner := newRaceGate([]branchResult{{name: "c"}, {name: "d"}}, func() {})
	inner.winner = 0

	_, err := holdArtifacts(withRaceSeat(withRaceSeat(t.Context(), outer, 1), inner, 1), true)
	if !errors.Is(err, errRaceLost) {
		t.Fatalf("holdArtifacts = %v, want errRaceLost from the inner race", err)
	}

	if outer.owner != -1 {
		t.Errorf("the outer race is held by %d after the inner one refused, want none", outer.owner)
	}
}
