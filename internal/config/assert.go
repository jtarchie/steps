package config

// The assert: directive in all three of its positions — pipeline (job
// names), job (step names), and step (stdout/exit code/verdict/tool-call
// trajectory).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Assert is a self-verification directive, in one of two shapes depending on
// where it's attached:
//   - On a Config (top level), only Execution is valid: an ordered list of the
//     job names that must have run.
//   - On a Job, Execution (task/agent/hook names) and Outcome. By omission
//     Execution also asserts what must NOT run. A matching Job Execution
//     clears the plan's failure, so one green fixture can contain
//     deliberately-failing tasks; Outcome is how a fixture says what the plan
//     concluded, which Execution structurally cannot.
//   - On a task/agent Step, only Stdout/Code are valid: Stdout is a substring
//     the step's captured output must contain, Code the exact expected exit
//     code (task only). A matching assert makes a non-zero-exit task a
//     success.
type Assert struct {
	Execution []string `yaml:"execution,omitempty"`
	// Outcome, on a Job, asserts what the plan CONCLUDED rather than which
	// steps ran: AssertOutcomeFailed or AssertOutcomeSucceeded. It exists
	// because execution: cannot express "this job should have failed" — a
	// matching execution: clears the plan's error, so a fixture for any defect
	// about error propagation or swallowing passes on the broken build and the
	// fixed build alike. See docs/control-flow.md.
	Outcome string  `yaml:"outcome,omitempty"`
	Stdout  *string `yaml:"stdout,omitempty"`
	Code    *int    `yaml:"code,omitempty"`
	// Verdict, on an agent step that declares verdicts:, asserts which one the
	// model emitted — an exact match against the declared vocabulary.
	//
	// It exists because a verdict is the one agent outcome a fixture could not
	// pin directly: stdout: tests the prose around the decision, and
	// tool_calls: could only assert that the verdict tool was CALLED, not what
	// it chose. A classifier whose whole product is the choice was therefore
	// untestable at the one place it matters.
	Verdict *string `yaml:"verdict,omitempty"`
	// ToolCalls, on an agent step, asserts the ordered trajectory of tool
	// calls the model made (see ExpectedToolCall). Agent-only: a task step
	// runs no tools. Every entry must appear, in order, as a subsequence of
	// the observed calls.
	ToolCalls []ExpectedToolCall `yaml:"tool_calls,omitempty"`
	// Files, on a task or agent step, asserts that every listed path exists
	// as a non-empty file in the step's own outputs — the one thing no other
	// assert field checks: what the step WROTE rather than what it SAID. Each
	// path is artifact-relative (ValidateArtifactPath), and its first
	// component must name one of the step's declared outputs:.
	Files []string `yaml:"files,omitempty"`
}

// The two legal values of a job's assert.outcome.
const (
	// AssertOutcomeSucceeded requires the plan to have produced no error. It
	// is not a no-op: it opts a job OUT of the rule that a matching
	// execution: clears whatever the plan produced.
	AssertOutcomeSucceeded = "succeeded"
	// AssertOutcomeFailed requires the plan to have produced an error, and
	// then clears it — the assertion is what makes the job green.
	AssertOutcomeFailed = "failed"
)

// ExpectedToolCall is one entry in an agent step's assert.tool_calls: the
// tool's name, plus (optionally) a subset of the arguments the model must
// have called it with. Args is a subset match — every listed key must be
// present with an equal value, and any extra actual argument is ignored.
// Values compare as strings, since every argument reaching a tool's run:
// template is rendered as one (this is a deliberate divergence from
// secret-agent's eval matcher, which coerces across int/float).
type ExpectedToolCall struct {
	Name string            `yaml:"name"`
	Args map[string]string `yaml:"args,omitempty"`
}

// validateAsserts enforces which Assert fields are valid where: a pipeline
// assert may only set execution:; a job assert execution: and outcome:; a
// task/agent step's assert only stdout:/code: (and code: only on tasks). A step
// assert is rejected on get/put steps. Hook steps are walked too (via
// visitSteps), so an assert on a hook task/agent gets the same treatment.
func (c *Config) validateAsserts() error {
	err := validatePipelineAssert(c.Assert)
	if err != nil {
		return err
	}

	for _, job := range c.Jobs {
		err = validateJobAssert(job.Name, job.Assert)
		if err != nil {
			return err
		}

		err = job.visitSteps(func(label string, step *Step) error {
			stepErr := rejectAssertInsideTry(label, step)
			if stepErr != nil {
				return stepErr
			}

			stepErr = validateStepAssert(label, step)
			if stepErr != nil {
				return stepErr
			}

			stepErr = c.validateAssertFiles(label, step)
			if stepErr != nil {
				return stepErr
			}

			return c.validateAssertPinnedArgs(label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAssertPinnedArgs rejects an assert.tool_calls entry that asserts on
// an argument the pipeline pins via a custom tool's args: (see ToolSpec.Args).
// A pinned value is machine-supplied and never appears among the
// model-authored arguments a trajectory records, so such an assert could never
// match — failing the load is far clearer than a step that always fails.
//
// Best-effort by design: it fires only when the agent resolves here. An
// unresolvable agent name is left to run time, matching how every other
// agent/task reference in this package is treated.
func (c *Config) validateAssertPinnedArgs(label string, step *Step) error {
	if step.Assert == nil || len(step.Assert.ToolCalls) == 0 || step.Agent == "" {
		return nil
	}

	agent, err := c.FindAgent(step.Agent)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable agent is caught at run time, same as everywhere else
	}

	pinned := pinnedArgsByTool(agent.Tools, step.Tools)

	for i, want := range step.Assert.ToolCalls {
		keys := make([]string, 0, len(want.Args))
		for key := range want.Args {
			keys = append(keys, key)
		}

		sort.Strings(keys) // deterministic message when several keys are pinned

		for _, key := range keys {
			if pinned[want.Name][key] {
				return fmt.Errorf("%s: assert.tool_calls[%d]: tool %q pins argument %q via args:, so it never appears in the model-authored call and can never match", label, i, want.Name, key)
			}
		}
	}

	return nil
}

// pinnedArgsByTool indexes which argument keys each named tool pins, across an
// agent's grant and a step's own inline tools.
func pinnedArgsByTool(agentTools, stepTools []ToolSpec) map[string]map[string]bool {
	index := map[string]map[string]bool{}

	add := func(specs []ToolSpec) {
		for _, spec := range specs {
			if len(spec.Args) == 0 {
				continue
			}

			name := ToolSpecName(spec)

			if index[name] == nil {
				index[name] = map[string]bool{}
			}

			for key := range spec.Args {
				index[name][key] = true
			}
		}
	}

	add(agentTools)
	add(stepTools)

	return index
}

// validatePipelineAssert checks the top-level assert:, which names job names
// and nothing else. outcome: has no meaning here — a pipeline runs many jobs
// and concludes once per job, not once overall.
func validatePipelineAssert(assert *Assert) error {
	if assert == nil {
		return nil
	}

	err := requireExecutionOnly("pipeline assert", assert)
	if err != nil {
		return err
	}

	if assert.Outcome != "" {
		return errors.New("pipeline assert: outcome is only valid on a job assert; a pipeline runs many jobs and has no single outcome")
	}

	return nil
}

// validateJobAssert checks a job's assert:, which may set execution: and
// outcome: but none of the step-only fields.
func validateJobAssert(jobName string, assert *Assert) error {
	if assert == nil {
		return nil
	}

	label := fmt.Sprintf("job %q assert", jobName)

	err := requireExecutionOnly(label, assert)
	if err != nil {
		return err
	}

	return validateAssertOutcome(label, assert.Outcome)
}

// validateAssertOutcome rejects any assert.outcome value other than the two
// legal ones. Absent is legal — it means today's behavior, where a job's
// conclusion is not asserted on at all.
func validateAssertOutcome(label, outcome string) error {
	switch outcome {
	case "", AssertOutcomeSucceeded, AssertOutcomeFailed:
		return nil
	default:
		return fmt.Errorf("%s: outcome %q is not valid; use %q or %q%s",
			label, outcome, AssertOutcomeSucceeded, AssertOutcomeFailed,
			suggestion(outcome, []string{AssertOutcomeSucceeded, AssertOutcomeFailed}))
	}
}

// requireExecutionOnly rejects an execution-level assert (Config/Job) that
// carries the step-only stdout:/code:/files: fields.
func requireExecutionOnly(label string, assert *Assert) error {
	if assert.Stdout != nil || assert.Code != nil || assert.Verdict != nil || len(assert.Files) > 0 {
		return fmt.Errorf("%s: stdout/code/verdict/files are only valid on task/agent step asserts, not an execution assert", label)
	}

	return nil
}

// rejectAssertInsideTry rejects assert: on a try: wrapper and on the step it
// wraps.
//
// assert: is the self-verification that turns a step into a `steps test`
// fixture, and try: exists to swallow exactly the failure such an assert
// reports — so an assert anywhere inside a try: can never fail a run. Left
// legal it is worse than dead config: a mismatch prints its got-vs-want line,
// the run continues, and `steps test` reports PASS on the broken fixture the
// assert was written to catch. Both halves are checked here because only the
// wrapper can see that its inner step is wrapped.
func rejectAssertInsideTry(label string, step *Step) error {
	if step.Try == nil {
		return nil
	}

	if step.Assert != nil || step.Try.Assert != nil {
		return fmt.Errorf("%s: assert is not valid on a try: step or the step it wraps — try: tolerates the failure, so the assertion could never fail the run", label)
	}

	return nil
}

// validateStepAssert rejects a step assert that's misplaced (get/put/try) or
// carries the wrong fields for its step kind.
//
//nolint:cyclop // the switch over step kinds is inherently branching
func validateStepAssert(label string, step *Step) error {
	if step.Assert == nil {
		return nil
	}

	if len(step.Assert.Execution) > 0 {
		return fmt.Errorf("%s: execution is only valid on job/pipeline asserts, not a step assert", label)
	}

	if step.Assert.Outcome != "" {
		return fmt.Errorf("%s: outcome is only valid on a job assert; a step's own success is asserted with stdout/code", label)
	}

	kind, ok := step.Kind()
	if !ok {
		return fmt.Errorf("%s: unrecognized step (must be get, task, put, or agent)", label)
	}

	switch kind { //nolint:exhaustive // default covers StepKindTask
	case StepKindGet:
		return fmt.Errorf("%s (get %q): assert is not valid on get steps", label, step.Get)
	case StepKindPut:
		return fmt.Errorf("%s (put %q): assert is not valid on put steps", label, step.Put)
	case StepKindTry:
		// Unreachable in practice — rejectAssertInsideTry runs first and
		// reports the wrapper and the wrapped step in one message.
		return fmt.Errorf("%s: assert is not valid on a try: step", label)
	case StepKindAgent:
		if step.Assert.Code != nil {
			return fmt.Errorf("%s (agent %q): assert.code is not valid on agent steps (no exit code); use assert.stdout", label, step.Agent)
		}

		err := validateAssertVerdict(fmt.Sprintf("%s (agent %q)", label, step.Agent), step)
		if err != nil {
			return err
		}

		return validateExpectedToolCalls(fmt.Sprintf("%s (agent %q)", label, step.Agent), step.Assert.ToolCalls)
	default: // StepKindTask
		if len(step.Assert.ToolCalls) > 0 {
			return fmt.Errorf("%s: assert.tool_calls is only valid on agent steps (a task runs no tools)", label)
		}

		if step.Assert.Verdict != nil {
			return fmt.Errorf("%s: assert.verdict is only valid on agent steps (a task emits no verdict)", label)
		}

		return nil
	}
}

// validateAssertVerdict rejects an assert.verdict that the step could never
// satisfy: one on an agent that declares no verdicts:, and one naming a
// verdict outside the declared vocabulary. Both are load errors rather than
// run-time failures because both are unconditional — the assert cannot match
// on any run, against any model.
func validateAssertVerdict(context string, step *Step) error {
	want := step.Assert.Verdict
	if want == nil {
		return nil
	}

	names := step.VerdictNames()
	if len(names) == 0 {
		return fmt.Errorf("%s: assert.verdict is set, but the step declares no verdicts: — there is no decision to assert on", context)
	}

	if !slices.Contains(names, *want) {
		return fmt.Errorf("%s: assert.verdict %q is not one of the declared verdicts (%s)%s",
			context, *want, strings.Join(names, ", "), suggestion(*want, names))
	}

	return nil
}

// validateExpectedToolCalls rejects an assert.tool_calls entry with no name —
// there is nothing to match against, and an empty name would silently match
// the first call of any tool.
func validateExpectedToolCalls(context string, expected []ExpectedToolCall) error {
	for i, want := range expected {
		if want.Name == "" {
			return fmt.Errorf("%s: assert.tool_calls[%d]: name is required", context, i)
		}
	}

	return nil
}

// AssertFilesMismatch reports the first assert.files entry that is not a
// non-empty regular file under dir, or nil when every entry checks out.
//
// dir is the step's own working directory, read before its outputs are
// captured — internal/pipeline calls it for a task step and internal/agent for
// an agent step, which is why it lives here: this package is the one they
// share, and it already owns the paired ValidateArtifactPath that made the
// paths safe to join in the first place.
func AssertFilesMismatch(files []string, dir string) error {
	for _, rel := range files {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			return fmt.Errorf("assert.files: %s does not exist", rel)
		}

		if info.IsDir() {
			return fmt.Errorf("assert.files: %s is a directory, not a file", rel)
		}

		if info.Size() == 0 {
			return fmt.Errorf("assert.files: %s is empty", rel)
		}
	}

	return nil
}

// validateAssertFiles rejects an assert.files entry whose path isn't
// artifact-relative (ValidateArtifactPath — no ../, no absolute path, no
// empty segment), or whose first component doesn't name one of the step's
// own outputs:. The latter is what makes a bare filename like "reply.md" a
// load error rather than a path that can never resolve to anything a step
// captures: context_paths and a put's params: file follow the same
// artifact-relative rule.
func (c *Config) validateAssertFiles(label string, step *Step) error {
	if step.Assert == nil || len(step.Assert.Files) == 0 {
		return nil
	}

	// Only task and agent steps produce outputs to check. Every other kind is
	// REJECTED rather than skipped: returning nil here made assert.files a
	// silent no-op on load_var:, approval: and the block kinds, so `validate`
	// passed, the assert never ran, and `steps test` reported PASS on a
	// fixture that checked nothing.
	kind, ok := step.Kind()
	if !ok {
		return fmt.Errorf("%s: assert.files needs a task or agent step; this step's kind is unrecognized", label)
	}

	if kind != StepKindTask && kind != StepKindAgent {
		return fmt.Errorf("%s: assert.files is only valid on a task or agent step, not %s — nothing else captures outputs to check", label, kind)
	}

	outputs, err := c.stepOutputs(*step)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}

	return checkAssertFilePaths(label, step.Assert.Files, outputs)
}

// checkAssertFilePaths is validateAssertFiles' per-path half, split out so
// each function stays within the cyclomatic budget.
func checkAssertFilePaths(label string, files, outputs []string) error {
	declared := make(map[string]bool, len(outputs))
	for _, name := range outputs {
		declared[name] = true
	}

	for i, path := range files {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("%s: assert.files[%d] must not be empty", label, i)
		}

		err := ValidateArtifactPath(path)
		if err != nil {
			return fmt.Errorf("%s: assert.files[%d]: %w", label, i, err)
		}

		name, rest, hasRest := strings.Cut(path, "/")
		if !declared[name] {
			return fmt.Errorf("%s: assert.files[%d] %q names artifact %q, which is not one of this step's outputs (%s)",
				label, i, path, name, strings.Join(outputs, ", "))
		}

		// A bare artifact name is the output DIRECTORY, and a directory can
		// never satisfy an assertion about a non-empty FILE — so this could
		// only ever fail, on every run, whatever the step wrote. Rejected at
		// load like every other unsatisfiable assert rather than at the end of
		// the step that was going to fail anyway.
		if !hasRest || strings.TrimSpace(rest) == "" {
			return fmt.Errorf("%s: assert.files[%d] %q names the whole %q output, which is a directory — name a file inside it (%s/...)",
				label, i, path, name, name)
		}
	}

	return nil
}

// stepOutputs resolves the declared output artifact names for a task or
// agent step. An agent's outputs: is authoritative on the step itself — an
// agents: entry declares no outputs of its own, unlike tasks: — but a task
// step may inherit its whole outputs: list from a named tasks: entry, so
// that side goes through ResolveTask rather than reading step.Outputs raw.
func (c *Config) stepOutputs(step Step) ([]string, error) {
	if step.Agent != "" {
		return step.Outputs, nil
	}

	rt, err := c.ResolveTask(step)
	if err != nil {
		return nil, err
	}

	return rt.Outputs, nil
}
