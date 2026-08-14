package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
)

// TestBlockBudgetUnbindable pins when a block's own ceiling is decorative.
//
// Two things have to hold. The width has to cover every cell — otherwise a
// slot is contended for, a cell finishes and reports, and admission decides on
// real spend. AND the reservations have to be too small to reach the ceiling
// by themselves: nothing has reported when the last cell is admitted, so the
// most any admission can see is what the other cells hold, and (cells-1)
// reservations that never reach the ceiling admit everyone by construction.
//
// A reservation existing is NOT enough, which is the trap this pins.
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
		// A reservation big enough that the cells holding it reach the ceiling
		// binds at any width: 5 * 200 = 1000 refuses the sixth.
		{"a reservation that reaches the ceiling binds", budgeted(200), 8, 6, false},
		// Even a reservation far too small to reach the ceiling on its own
		// binds, because admission PAUSES once it is committed and the next
		// decision is made on what the finished cells really spent.
		{"a small reservation still binds", budgeted(1000 / 6), 6, 6, false},
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
// reservation every cell is admitted against a total of ~0.
//
// With one, the allowance is consumed up front. A cell that cannot fit is not
// refused outright — its reservation may yet be released — so this drives the
// PERMANENT half: once real spend reaches the ceiling, admission is over.
func TestBlockBudgetReservesOnAdmit(t *testing.T) {
	t.Parallel()

	// Room for exactly three cells at 200 apiece.
	spend, usage := newTestBlockBudget(t, 600, 200)

	for i := range 3 {
		if !spend.admit() {
			t.Fatalf("cell %d refused, want admitted: nothing has finished, so only reservations bound it", i)
		}
	}

	// They finish having spent the whole allowance.
	for range 3 {
		usage.Add(agent.StepUsage{Total: 200})
		spend.settle()
	}

	if spend.admit() {
		t.Error("a fourth cell was admitted after the allowance was spent outright")
	}
}

// TestBlockBudgetAdmitWaitsForAReservation pins the difference between the two
// kinds of refusal, which is the whole correctness of admission.
//
// Spend only grows, so a spend-driven refusal is permanent. Reservations are
// released as cells finish, so a reservation-driven one is not — treating it
// as permanent truncated matrices nowhere near their ceiling. This admits to
// capacity, then proves the next admission BLOCKS until a settle rather than
// coming back false.
func TestBlockBudgetAdmitWaitsForAReservation(t *testing.T) {
	t.Parallel()

	spend, usage := newTestBlockBudget(t, 600, 200)

	for range 3 {
		spend.admit()
	}

	admitted := make(chan bool, 1)
	go func() { admitted <- spend.admit() }()

	select {
	case got := <-admitted:
		t.Fatalf("admit returned %v immediately; it must wait while only reservations stand in the way", got)
	case <-time.After(50 * time.Millisecond):
	}

	// A cell finishes far under its reservation, which frees the allowance.
	usage.Add(agent.StepUsage{Total: 5})
	spend.settle()

	select {
	case got := <-admitted:
		if !got {
			t.Error("admit refused after a reservation was released with almost nothing spent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admit never woke after settle")
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

// TestBlockBudgetRealSpendCatchesAnOverrun is the cost-safety property the
// reservation alone does NOT provide, and the reason a width below the cell
// count matters.
//
// A reservation is an assumption. When cells overrun it, what stops the block
// is the next wave reading their REAL spend — which only exists because the
// width forced them to finish first. Mirrors examples/pr-review.yml: three at
// a time, 900K reserved, each actually costing 1.2M against a 3.6M ceiling.
func TestBlockBudgetRealSpendCatchesAnOverrun(t *testing.T) {
	t.Parallel()

	spend, usage := newTestBlockBudget(t, 3_600_000, 900_000)

	for i := range 3 {
		if !spend.admit() {
			t.Fatalf("wave-1 cell %d refused with nothing spent", i)
		}
	}

	// Each overruns its reservation by a third.
	for range 3 {
		usage.Add(agent.StepUsage{Total: 1_200_000})
		spend.settle()
	}

	if spend.admit() {
		t.Error("admitted a second wave after the first spent the whole allowance; the overrun went uncaught")
	}
}
