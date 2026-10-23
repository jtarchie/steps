package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
)

// TestFanOutRecoversAPanicAsThatItemsError: one item panicking must not crash the process, must surface as its error, and must leave the others (including ones waiting on its slot) to finish.
func TestFanOutRecoversAPanicAsThatItemsError(t *testing.T) {
	t.Parallel()

	var (
		ran    atomic.Int32
		panics = make([]error, 4)
	)

	fanOut(t.Context(), 4, 1,
		func(int) bool { return true },
		func(_ context.Context, index int) {
			if index == 1 {
				panic("boom")
			}

			ran.Add(1)
		},
		func(index int, err error) { panics[index] = err },
	)

	if got := ran.Load(); got != 3 {
		t.Errorf("%d items ran to completion, want 3: a panic must release its slot so later items still run", got)
	}

	for index, err := range panics {
		if (err != nil) != (index == 1) {
			t.Errorf("item %d panic error = %v", index, err)
		}
	}

	if !strings.Contains(panics[1].Error(), "boom") {
		t.Errorf("panic error %q does not carry the panic value", panics[1])
	}
}

// TestInParallelBranchPanicIsThatBranchsError: through the real block, a panicking branch is an errored branch, not a dead daemon.
func TestInParallelBranchPanicIsThatBranchsError(t *testing.T) {
	t.Parallel()

	step := config.Step{InParallel: &config.InParallel{Steps: []config.Step{{Task: "a"}, {Task: "b"}}}}

	// A zero stepRunner has no store, so runNonGetStep dereferences nil: the stand-in for any bug inside a branch.
	results := runBranches(t.Context(), stepRunner{}, 0, step, "hash")

	for _, result := range results {
		if result.err == nil {
			t.Fatalf("branch %d reported success after panicking", result.index)
		}

		if got := outcome.Classify(t.Context(), result.err); got != outcome.Errored {
			t.Errorf("branch %d classified %v, want errored: a panic is infrastructure, not a step saying no", result.index, got)
		}
	}
}

// TestEnsembleStopsAdmittingMembersPastTheJobDeadline: a job timeout bounds an ensemble like it does in_parallel, and a member never admitted is an errored vote rather than a zero-value one the verdict could mistake for an abstention.
func TestEnsembleStopsAdmittingMembersPastTheJobDeadline(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(t.Context(), jobDeadlineKey{}, jobDeadline{at: time.Now().Add(-time.Hour), timeout: time.Minute})
	step := config.Step{Ensemble: &config.Ensemble{Agents: []config.Step{{Agent: "a"}, {Agent: "b"}}}}

	votes := runEnsembleMembers(ctx, stepRunner{jobName: "j"}, 0, step, "hash")

	for _, vote := range votes {
		if !errors.Is(vote.err, errMemberNotAdmitted) {
			t.Errorf("member %q was never admitted yet its vote is %v", vote.agent, vote.err)
		}
	}

	if votes[1].agent != "b" {
		t.Errorf("unadmitted member lost its name: %q", votes[1].agent)
	}

	_, err := countableVotes(&config.Ensemble{}, votes)
	if err == nil {
		t.Error("an ensemble where no member ran still produced a countable verdict")
	}
}
