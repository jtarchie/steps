package pipeline

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
)

// TestBlockBudgetUnbindable pins when a block's own ceiling is decorative.
//
// A budget with a reservation binds at any width — admission consumes the
// allowance up front. The hole the warning survives for is a budget with NO
// reservation source at a width covering every cell: admission only ever sees
// FINISHED cells' spend, a cell only finishes once something waits for its
// slot, and at that width there is no such wait anywhere in the block
// (newLimiter hands back a limiter that never blocks). Every cell is admitted
// against a total of ~0 and the budget stops nothing, so the run says so out
// loud instead of letting the number look like a ceiling.
func TestBlockBudgetUnbindable(t *testing.T) {
	t.Parallel()

	budgeted := func(reserve int) *blockBudget {
		ctx := agent.WithRunUsage(context.Background(), agent.NewRunUsage(0))
		step := config.Step{Budget: &config.Budget{Tokens: 1000, ReservePerCell: reserve}}

		return newBlockBudget(ctx, &config.Config{}, step, nil)
	}

	tests := []struct {
		name        string
		spend       *blockBudget
		maxInFlight int
		cells       int
		want        bool
	}{
		{"a width under the cell count binds", budgeted(0), 2, 6, false},
		{"a width equal to the cell count cannot bind", budgeted(0), 6, 6, true},
		{"a width above the cell count cannot bind", budgeted(0), 8, 6, true},
		{"no budget is never reported", &blockBudget{}, 8, 6, false},
		// With a reservation the width no longer matters: admission takes the
		// allowance before any cell reports, so the ceiling binds.
		{"a reservation binds at any width", budgeted(200), 8, 6, false},
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

// newTestBlockBudget builds a block budget over a fresh accumulator, and
// returns it alongside the accumulator so a test can report spend the way a
// finished cell does.
func newTestBlockBudget(t *testing.T, ceiling, reserve int) (*blockBudget, *agent.RunUsage) {
	t.Helper()

	usage := agent.NewRunUsage(0)
	ctx := agent.WithRunUsage(context.Background(), usage)
	step := config.Step{Budget: &config.Budget{Tokens: ceiling, ReservePerCell: reserve}}

	return newBlockBudget(ctx, &config.Config{}, step, nil), usage
}

// TestBlockBudgetReservesOnAdmit is the case a spent()-only ceiling could
// never bind: nothing has finished, so nothing has reported, and without a
// reservation every cell is admitted against a total of ~0. With one, the
// allowance is consumed up front and the block stops admitting at the line.
func TestBlockBudgetReservesOnAdmit(t *testing.T) {
	t.Parallel()

	// Room for exactly three cells at 200 apiece; the fourth crosses.
	spend, _ := newTestBlockBudget(t, 600, 200)

	for i := range 3 {
		if !spend.admit() {
			t.Fatalf("cell %d refused, want admitted: nothing has finished, so only reservations bound it", i)
		}
	}

	if spend.admit() {
		t.Error("the fourth cell was admitted; three reservations of 200 already cover the 600 ceiling")
	}
}

// TestBlockBudgetSettleReleasesUnderspend is why this is a reservation and not
// a hard pre-allocation: a cell that comes in under its reserve hands the
// difference back, so later cells the ceiling would otherwise have refused
// still run.
func TestBlockBudgetSettleReleasesUnderspend(t *testing.T) {
	t.Parallel()

	spend, usage := newTestBlockBudget(t, 600, 200)

	// Three admitted, allowance fully reserved.
	for range 3 {
		spend.admit()
	}

	if spend.admit() {
		t.Fatal("precondition: a fourth cell must be refused while three reservations stand")
	}

	// All three finish having spent 10 each, not the 200 assumed.
	for range 3 {
		usage.Add(agent.StepUsage{Total: 10})
		spend.settle()
	}

	if !spend.admit() {
		t.Error("a cell was refused after three cells came in far under reserve; settle must return the difference")
	}
}

// TestBlockBudgetAdmitsWithoutReservationAsBefore pins the fallback: a block
// with no reservation source admits on spent() alone, exactly as it did
// before reservations existed. An existing pipeline must not change behaviour
// because a new admission mode wants a number it never asked for.
func TestBlockBudgetAdmitsWithoutReservationAsBefore(t *testing.T) {
	t.Parallel()

	spend, usage := newTestBlockBudget(t, 600, 0)

	for i := range 5 {
		if !spend.admit() {
			t.Fatalf("cell %d refused with nothing spent and no reservation, want admitted", i)
		}

		spend.settle()
	}

	usage.Add(agent.StepUsage{Total: 600})

	if spend.admit() {
		t.Error("admitted a cell after the ceiling was spent outright")
	}
}
