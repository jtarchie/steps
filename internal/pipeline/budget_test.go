package pipeline

import (
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/agent"
)

// Spend landing exactly on the ceiling is a permanent refusal: spend only grows, so waiting on a sibling still in flight can never change the answer.
func TestBlockBudgetSpendAtTheCeilingRefusesWithoutWaiting(t *testing.T) {
	t.Parallel()

	spend, usage := newTestBlockBudget(t, 600, 0)

	if !spend.admit() {
		t.Fatal("first cell refused with nothing spent")
	}

	usage.Add(agent.StepUsage{Total: 600})

	admitted := make(chan bool, 1)
	go func() { admitted <- spend.admit() }()

	select {
	case got := <-admitted:
		if got {
			t.Error("admitted a cell with the whole allowance spent")
		}
	case <-time.After(5 * time.Second):
		spend.settle()
		<-admitted
		t.Fatal("admit waited on an in-flight cell though spend alone had reached the ceiling")
	}
}
