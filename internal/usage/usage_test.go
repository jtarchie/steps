package usage

import "testing"

func TestRunUsageCeilingIsReachableNotExceeded(t *testing.T) {
	t.Parallel()

	job := NewRun(100)

	if job.WouldExceed(100) {
		t.Error("100 pending of a 100-token job budget would exceed it")
	}

	if job.Add(Step{Step: "a", Total: 100}) {
		t.Error("spending exactly the 100-token job budget exceeded it")
	}

	if !job.WouldExceed(1) {
		t.Error("one token past the job budget would not exceed it")
	}

	if !job.Add(Step{Step: "b", Total: 1}) {
		t.Error("one token past the job budget did not exceed it")
	}

	unbounded := NewRun(0)

	if unbounded.WouldExceed(1) || unbounded.Add(Step{Step: "a", Total: 1}) {
		t.Error("a job with no budget tripped a ceiling")
	}
}

// TestRunUsageWithoutABudgetNeverTrips keeps reporting free of enforcement.
func TestRunUsageWithoutABudgetNeverTrips(t *testing.T) {
	t.Parallel()

	run := NewRun(0)

	if run.Add(Step{Step: "planner", Total: 1_000_000}) {
		t.Error("a job with no budget reported a breach")
	}

	if run.Total() != 1_000_000 {
		t.Errorf("Total() = %d, want 1000000 — usage is counted regardless", run.Total())
	}

	if len(run.Steps()) != 1 {
		t.Errorf("Steps() = %d entries, want 1", len(run.Steps()))
	}
}
