package pipeline

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
)

// TestBlockBudgetUnbindable pins when a block's own ceiling is decorative.
//
// An admission-time ceiling only ever sees FINISHED cells' spend, and a cell
// only finishes once something waits for its slot. At a width covering every
// cell there is no such wait anywhere in the block — newLimiter hands back a
// limiter that never blocks — so every cell is admitted against a total of ~0
// and the budget stops nothing. That is inherent to bounding what gets
// STARTED, so the run says so out loud instead of letting the number look like
// a ceiling.
func TestBlockBudgetUnbindable(t *testing.T) {
	t.Parallel()

	budgeted := func() *blockBudget {
		ctx := agent.WithRunUsage(context.Background(), agent.NewRunUsage(0))

		return newBlockBudget(ctx, config.Step{Budget: &config.Budget{Tokens: 1000}})
	}

	tests := []struct {
		name        string
		spend       *blockBudget
		maxInFlight int
		cells       int
		want        bool
	}{
		{"a width under the cell count binds", budgeted(), 2, 6, false},
		{"a width equal to the cell count cannot bind", budgeted(), 6, 6, true},
		{"a width above the cell count cannot bind", budgeted(), 8, 6, true},
		{"no budget is never reported", &blockBudget{}, 8, 6, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := test.spend.unbindable(test.maxInFlight, test.cells); got != test.want {
				t.Errorf("unbindable(%d, %d) = %v, want %v", test.maxInFlight, test.cells, got, test.want)
			}
		})
	}
}
