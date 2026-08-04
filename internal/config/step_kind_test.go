package config

import "testing"

func TestStepKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		step     Step
		wantKind StepKind
		wantOK   bool
	}{
		{name: "get", step: Step{Get: "thing"}, wantKind: StepKindGet, wantOK: true},
		{name: "task", step: Step{Task: "build"}, wantKind: StepKindTask, wantOK: true},
		{name: "put", step: Step{Put: "thing"}, wantKind: StepKindPut, wantOK: true},
		{name: "agent", step: Step{Agent: "reviewer"}, wantKind: StepKindAgent, wantOK: true},
		{name: "try", step: Step{Try: &Step{Task: "build", Run: "echo"}}, wantKind: StepKindTry, wantOK: true},
		{name: "zero kinds set", step: Step{}, wantOK: false},
		{name: "two kinds set (task and put)", step: Step{Task: "build", Put: "thing"}, wantOK: false},
		{name: "two kinds set (get and agent)", step: Step{Get: "thing", Agent: "reviewer"}, wantOK: false},
		{name: "two kinds set (try and task)", step: Step{Try: &Step{Task: "build"}, Task: "build"}, wantOK: false},
		{name: "three kinds set", step: Step{Task: "build", Put: "thing", Agent: "reviewer"}, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kind, ok := tt.step.Kind()
			if ok != tt.wantOK {
				t.Fatalf("Kind() ok = %v, want %v", ok, tt.wantOK)
			}

			if ok && kind != tt.wantKind {
				t.Errorf("Kind() = %q, want %q", kind, tt.wantKind)
			}
		})
	}
}
