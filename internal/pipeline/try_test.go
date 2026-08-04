package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
)

func tryStep(inner config.Step) config.Step {
	return config.Step{Try: &inner}
}

// TestTolerateTryFailureClassifies pins the line try: draws. Swallowing every
// error class is what let a Ctrl-C mid-step report a green job and exit 0,
// and let a docker/transport outage march the plan into the next step.
func TestTolerateTryFailureClassifies(t *testing.T) {
	t.Parallel()

	failed := outcome.Fail(errors.New("exit status 1"))
	errored := errors.New("docker: cannot connect")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, testCase := range []struct {
		name      string
		ctx       context.Context //nolint:containedctx // the classification under test is ctx-dependent
		step      config.Step
		err       error
		tolerated bool
	}{
		{name: "task-level failure", ctx: context.Background(), step: tryStep(config.Step{Task: "notify"}), err: failed, tolerated: true},
		{name: "infrastructure error", ctx: context.Background(), step: tryStep(config.Step{Task: "notify"}), err: errored},
		{name: "abort", ctx: canceled, step: tryStep(config.Step{Task: "notify"}), err: failed},
		{name: "success", ctx: context.Background(), step: tryStep(config.Step{Task: "notify"}), err: nil, tolerated: true},
		{name: "not a try step", ctx: context.Background(), step: config.Step{Task: "notify"}, err: failed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := tolerateTryFailure(testCase.ctx, "job", testCase.step, testCase.err)
			if (got == nil) != testCase.tolerated {
				t.Errorf("tolerateTryFailure(%v) = %v, tolerated=%v", testCase.err, got, testCase.tolerated)
			}
		})
	}
}

// TestRunHookStepToleratesTryBody covers a try: used as a hook body. runHooks
// PROMOTES a failing on_success/ensure hook into the step's error on an
// otherwise green outcome, so without toleration here the best-effort notifier
// docs/control-flow.md advertises turned a green build red — the exact outcome
// try: exists to prevent.
func TestRunHookStepToleratesTryBody(t *testing.T) {
	t.Parallel()

	markers := filepath.Join(t.TempDir(), "markers")
	scope := hookTestScope(t, markers)
	scope.cfg.Tasks = append(scope.cfg.Tasks, config.Task{Name: "notify", Run: "echo notify >> " + markers + "; exit 1"})

	hooks := config.Hooks{Ensure: &config.Step{Try: &config.Step{Task: "notify"}}}

	err := runHooks(context.Background(), scope, hooks, nil)
	if err != nil {
		t.Errorf("a failing try: ensure hook must not fail its green step: %v", err)
	}

	if got := readMarkers(t, markers); !slices.Equal(got, []string{"notify"}) {
		t.Errorf("markers = %v, want [notify] (the hook body must still run)", got)
	}
}

// TestRunHookStepPropagatesUnwrappedFailure is the control for the above: an
// unwrapped hook body still fails its green step.
func TestRunHookStepPropagatesUnwrappedFailure(t *testing.T) {
	t.Parallel()

	markers := filepath.Join(t.TempDir(), "markers")
	scope := hookTestScope(t, markers)
	scope.cfg.Tasks = append(scope.cfg.Tasks, config.Task{Name: "notify", Run: "echo notify >> " + markers + "; exit 1"})

	hooks := config.Hooks{Ensure: &config.Step{Task: "notify"}}

	err := runHooks(context.Background(), scope, hooks, nil)
	if err == nil {
		t.Error("a failing ensure hook with no try: must fail its green step")
	}
}

// TestRecordStepExecutionSkipsTryWrapper guards against double-counting: both
// the wrapper and the step it wraps answer executedStepName with the same
// name, so recording both would put one execution in a job's assert.execution
// twice — and recording only the wrapper would claim an execution for a
// when:-guarded inner step that never ran.
func TestRecordStepExecutionSkipsTryWrapper(t *testing.T) {
	t.Parallel()

	log := &execLog{}
	ctx := withExecLog(context.Background(), log)

	recordStepExecution(ctx, tryStep(config.Step{Task: "notify"}))
	recordStepExecution(ctx, config.Step{Task: "notify"})

	if got := log.snapshot(); !slices.Equal(got, []string{"notify"}) {
		t.Errorf("execution log = %v, want [notify]", got)
	}
}

// TestHandoffForLooksThroughTry: handoff: sits on the agent step, which a try:
// wrapper hides. Reading it off the wrapper (which is load-time forbidden from
// carrying one) meant a tolerated agent was always entered with a nil Handoff
// and answered a redo as if freshly started.
func TestHandoffForLooksThroughTry(t *testing.T) {
	t.Parallel()

	pending := &agent.Handoff{JobName: "job", FromStep: "judge", RouteKey: "fail"}
	step := tryStep(config.Step{Agent: "reviewer", Handoff: &config.HandoffSpec{Context: true}})

	if handoffFor(step, pending) != pending {
		t.Error("handoffFor must deliver the carry to an agent wrapped in try:")
	}

	if handoffFor(tryStep(config.Step{Agent: "reviewer"}), pending) != nil {
		t.Error("a wrapped agent without handoff: must not receive the carry")
	}
}
