package pipeline

// A get after a put: what each put of the plan recorded in this run, so the
// get that follows it fetches that version instead of one a check found.

import (
	"context"
	"fmt"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
)

// putOutcome is what a plan put left for the get after it.
type putOutcome int

const (
	// putRecorded: the put ran and printed a version.
	putRecorded putOutcome = iota
	// putSkipped: its when: (or a block's around it) was false.
	putSkipped
	// putFailed: it failed, and a try: carried the plan on.
	putFailed
	// putNoVersion: it succeeded and printed no version.
	putNoVersion
)

// putRecord is the last put to one resource, and the plan step that ran it.
type putRecord struct {
	step    int
	outcome putOutcome
	version map[string]any
}

// putRecords is the run's puts by resource, and which gets follow which put.
type putRecords struct {
	bindings map[string]config.PutBinding

	mu sync.Mutex
	by map[string]putRecord
}

type putRecordsKey struct{}

type putStepKey struct{}

func withPutRecords(ctx context.Context, job *config.Job) context.Context {
	return context.WithValue(ctx, putRecordsKey{}, &putRecords{bindings: job.PutBindings(), by: map[string]putRecord{}})
}

func putRecordsFrom(ctx context.Context) *putRecords {
	records, _ := ctx.Value(putRecordsKey{}).(*putRecords)

	return records
}

// withPutStep names the plan step whatever runs under ctx belongs to, so a put
// inside it records under that step; -1 is none, which a hook's body gets: a
// hook only reacts to its step, so its put is never the one a get follows.
func withPutStep(ctx context.Context, step int) context.Context {
	return context.WithValue(ctx, putStepKey{}, step)
}

func putStepFrom(ctx context.Context) (int, bool) {
	step, ok := ctx.Value(putStepKey{}).(int)

	return step, ok && step >= 0
}

// putBindingFor is the put a get follows, if it follows one.
func putBindingFor(ctx context.Context, step config.Step) (config.PutBinding, bool) {
	records := putRecordsFrom(ctx)
	if records == nil || step.Get == "" {
		return config.PutBinding{}, false
	}

	binding, ok := records.bindings[step.Get]

	return binding, ok
}

// boundGetNames is the gets of a job that follow a put: none of them is an
// input of the build, since no check chooses their version.
func boundGetNames(job *config.Job) map[string]bool {
	names := map[string]bool{}
	for name := range job.PutBindings() {
		names[name] = true
	}

	return names
}

// recordPut files what a plan put left for the get after it, and keeps a
// version under each get that follows this put, so a resume that skips the
// put still binds it.
func recordPut(ctx context.Context, st store.Versions, step config.Step, outcome putOutcome, version map[string]any) {
	records := putRecordsFrom(ctx)

	planStep, ok := putStepFrom(ctx)
	if records == nil || !ok {
		return
	}

	resource := step.PutResourceName()

	if outcome == putRecorded && len(version) == 0 {
		outcome = putNoVersion
	}

	records.mu.Lock()
	records.by[resource] = putRecord{step: planStep, outcome: outcome, version: version}
	records.mu.Unlock()

	if outcome != putRecorded {
		return
	}

	for _, binding := range records.bindings {
		if binding.Resource == resource && binding.PutStep == planStep {
			recordRunInput(ctx, st, binding.Get, resource, version)
		}
	}
}

// recordPutsSkipped files every put a skipped step would have run as skipped.
func recordPutsSkipped(ctx context.Context, step config.Step) {
	if putRecordsFrom(ctx) == nil {
		return
	}

	eachPlanPut(step, func(put config.Step) { recordPut(ctx, nil, put, putSkipped, nil) })
}

// eachPlanPut calls fn for every put step runs, hooks left out.
func eachPlanPut(step config.Step, fn func(config.Step)) {
	if step.Put != "" {
		fn(step)
	}

	if step.Try != nil {
		eachPlanPut(*step.Try, fn)
	}

	for _, branch := range merkle.ContainerBranches(step) {
		eachPlanPut(branch, fn)
	}
}

// errPutRecordedNothing is a get after a put whose put left no version.
type errPutRecordedNothing struct {
	binding config.PutBinding
	why     string
}

func (e errPutRecordedNothing) Error() string {
	return fmt.Sprintf("get %q: put %s (step %d) %s, so there is no version to fetch", e.binding.Get, e.binding.Put, e.binding.PutStep, e.why)
}

// putVersion is the version a get after a put fetches: what its put recorded
// in this run, else — the put was skipped by a resume — what the attempt that
// ran it kept. skipped is true when the put's when: skipped it, and the get
// goes with it.
func putVersion(ctx context.Context, st store.Versions, binding config.PutBinding) (map[string]any, bool, error) {
	if records := putRecordsFrom(ctx); records != nil {
		records.mu.Lock()
		record, ok := records.by[binding.Resource]
		records.mu.Unlock()

		if ok && record.step == binding.PutStep {
			switch record.outcome {
			case putRecorded:
				return record.version, false, nil
			case putSkipped:
				return nil, true, nil
			case putFailed:
				return nil, false, errPutRecordedNothing{binding, "failed"}
			case putNoVersion:
				return nil, false, errPutRecordedNothing{binding, "printed no version"}
			}
		}
	}

	version, kept, err := keptPutVersion(ctx, st, binding)
	if err != nil || kept {
		return version, false, err
	}

	return nil, false, errPutRecordedNothing{binding, "did not run in this build"}
}

// keptPutVersion is the version an earlier attempt of this run (or, for a
// replay, the run it forked) kept for the get.
func keptPutVersion(ctx context.Context, st store.Versions, binding config.PutBinding) (map[string]any, bool, error) {
	resume := resumeFrom(ctx)
	if resume == nil || !resume.resuming {
		return nil, false, nil
	}

	run := resume.id
	if resume.forkedFrom != "" {
		run = resume.forkedFrom
	}

	inputs, err := st.RunInputs(ctx, run)
	if err != nil {
		return nil, false, fmt.Errorf("could not read what run %q kept for get %q: %w", run, binding.Get, err)
	}

	for _, input := range inputs {
		if input.Input != binding.Get {
			continue
		}

		version, err := store.DecodeVersion(input.Version)
		if err != nil {
			return nil, false, fmt.Errorf("run %q kept get %q at a version that does not decode: %w", run, binding.Get, err)
		}

		return version, true, nil
	}

	return nil, false, nil
}

// withoutBoundInputs drops what a run kept for its gets after puts: those are
// no input of the build, and a resume or rerun rebinds them from the put.
func withoutBoundInputs(inputs []store.RunInput, job *config.Job) []store.RunInput {
	bound := boundGetNames(job)
	kept := inputs[:0:0]

	for _, input := range inputs {
		if !bound[input.Input] {
			kept = append(kept, input)
		}
	}

	return kept
}

type putSourceKey struct{}

// withPutSource names the put a get's fetch binds, for the line that says so.
func withPutSource(ctx context.Context, put string) context.Context {
	return context.WithValue(ctx, putSourceKey{}, put)
}

func putSourceFrom(ctx context.Context) string {
	put, _ := ctx.Value(putSourceKey{}).(string)

	return put
}

// checkedPlan is the plan without its gets after puts, which are no input of
// the build: their put, not a check, chooses their version.
func checkedPlan(job *config.Job) []config.Step {
	bound := boundGetNames(job)
	plan := make([]config.Step, 0, len(job.Plan))

	for _, step := range job.Plan {
		if step.Get == "" || !bound[step.Get] {
			plan = append(plan, step)
		}
	}

	return plan
}
