package workspace

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/jtarchie/steps/internal/config"
)

// ValidateArtifactFlow statically checks that every task/agent/put step's
// declared inputs name an artifact available by that point in the plan (a
// resource an earlier get fetches, or an output an earlier task/agent
// produces), plus an agent step's dir: (which names the artifact it works in).
// It runs for every job, so a step that declares inputs: [x] where nothing
// produces x — the classic "this job never fetched anything" mistake — fails
// here rather than obscurely at run time. It runs no check/in/out command —
// unlike PlanChains, it is always safe to run, even under --force, which skips
// PlanChains entirely. get's version:every fans out per resolved version but
// never changes which artifact *names* exist, so a single linear walk over the
// plan covers every branch that fan-out could produce.
//
// Crucially, a get starts a *fresh* triggered build (see runTriggeredBuild)
// whose artifact store is empty except for the resource it fetches — it does
// not inherit artifacts produced before it. So a get resets the available
// set to just its own resource rather than adding to it; otherwise this
// check would pass an input referencing a pre-get artifact that the runtime
// can't actually see, exactly the late failure it exists to prevent.
func ValidateArtifactFlow(cfg *config.Config, job *config.Job) error {
	available := map[string]bool{}

	for i, step := range job.Plan {
		err := validateStepArtifactFlow(cfg, job.Name, i, step, available)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateStepArtifactFlow checks one step's declared inputs against
// available (the artifact names produced by earlier steps in the same
// walk) and, for task/agent steps, adds this step's own declared outputs to
// available for steps after it. It also validates the step's hooks: an
// on_success hook sees the step's post-outputs view (the step ran and captured
// its outputs), while failure-path hooks (on_failure/on_error/on_abort/ensure)
// see only the pre-outputs view, since the step may have failed before
// producing anything.
func validateStepArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, available map[string]bool) error {
	// across: is a MODIFIER, so a from_file: axis can sit on a step of any
	// kind. Checked before the kind dispatch, once, rather than in each arm —
	// and here rather than at the top-level walk, so a matrix nested in a try:,
	// a do:, or a branch is reached by the same recursion everything else is.
	err := checkAcrossFromFileAvailable(jobName, i, step, available)
	if err != nil {
		return err
	}

	return validateStepKindArtifactFlow(cfg, jobName, i, step, available)
}

// validateStepKindArtifactFlow is validateStepArtifactFlow's kind dispatch,
// split out so neither half carries the other's branch count.
func validateStepKindArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, available map[string]bool) error {
	//kindswitch:ignore the container kinds are handled together in the default, via blockBranches
	switch {
	case step.Get != "":
		// A get inside an existing build fetches into the same workspace,
		// so its resource accumulates alongside earlier artifacts.
		// Failure-path hooks run before the fetch has landed anything, so
		// their pre view is empty.
		pre := map[string]bool{}
		available[step.Get] = true
		available[fetchedByGet(step.Get)] = true

		return validateStepHooks(cfg, jobName, i, step, pre, maps.Clone(available))
	case step.Put != "":
		return validatePutArtifactFlow(cfg, jobName, i, step, available)
	case step.Task != "":
		return validateTaskArtifactFlow(cfg, jobName, i, step, available)
	case step.Agent != "":
		return validateAgentArtifactFlow(cfg, jobName, i, step, available)
	case step.Try != nil:
		return validateTryArtifactFlow(cfg, jobName, i, step, available)
	case step.Do != nil:
		return validateDoArtifactFlow(cfg, jobName, i, step, available)
	case step.LoadVar != "":
		return validateLoadVarArtifactFlow(cfg, jobName, i, step, available)
	default:
		// A concurrent block (in_parallel:, race:, ensemble:) validates its
		// branches; anything else declares no artifacts of its own.
		//
		// A race's branches all declare the SAME outputs (config rejects
		// anything else) and only the winner's are produced, so the block's
		// effect on the view is one branch's rather than the union — which
		// falls out of the shared walk without a special case.
		branches := blockBranches(step)
		if branches == nil {
			return nil
		}

		return validateBlockArtifactFlow(cfg, jobName, i, step, branches, available)
	}
}

// validateTryArtifactFlow walks the step a try: wraps.
//
// try: only changes whether a FAILURE stops the plan; it changes nothing about
// artifacts, so the wrapped step's inputs are checked and its outputs
// published exactly as if it were unwrapped. Treating it as an ordinary
// container instead made a try-wrapped producer invisible here, and the next
// step naming its output failed static validation with "not a resource
// fetched or an output produced earlier".
func validateTryArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, available map[string]bool) error {
	pre := maps.Clone(available)

	err := validateStepArtifactFlow(cfg, jobName, i, *step.Try, available)
	if err != nil {
		return err
	}

	return validateStepHooks(cfg, jobName, i, step, pre, maps.Clone(available))
}

// validateLoadVarArtifactFlow checks a load_var: step the same way every other
// consuming kind is checked: its declared inputs must exist, and the file: it
// reads must live inside one of them.
//
// A load_var step produces no artifact — it captures a value into the run's
// var map — but it READS one, out of a directory materialized from its own
// inputs (see internal/pipeline's runLoadVarStep). Without this the classic
// `task: pick-tag` / `load_var: tag, file: version.txt` pair validated clean
// and then died mid-run on a file nothing had materialized.
func validateLoadVarArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, available map[string]bool) error {
	pre := maps.Clone(available)

	err := checkInputsAvailable(jobName, i, "load_var", step.LoadVar, step.InputNames(), available)
	if err != nil {
		return err
	}

	err = checkVarFileDeclared(jobName, i, step)
	if err != nil {
		return err
	}

	return validateStepHooks(cfg, jobName, i, step, pre, maps.Clone(available))
}

// checkVarFileDeclared validates that a load_var:'s file: names one of the
// step's own declared inputs by its first path component — the counterpart of
// checkDirDeclared for an agent's dir:. A bare `file: version.txt` names no
// artifact at all, which under per-step isolation can only ever be a missing
// file: nothing but declared artifacts is materialized at the root of a step's
// directory.
func checkVarFileDeclared(jobName string, i int, step config.Step) error {
	root := firstPathComponent(step.VarFile)
	if root == "" || root == "." {
		return nil
	}

	inputs := step.InputNames()
	for _, name := range inputs {
		if name == root {
			return nil
		}
	}

	if len(inputs) == 0 {
		return fmt.Errorf("job %q step %d (load_var %q): file %q is not inside a declared input; a step's directory holds only the artifacts it declares, so name the artifact holding it (file: <artifact>/%s, inputs: [<artifact>])",
			jobName, i, step.LoadVar, step.VarFile, step.VarFile)
	}

	return fmt.Errorf("job %q step %d (load_var %q): file %q names artifact %q, which the step does not declare in inputs: — only declared artifacts are materialized into its working directory",
		jobName, i, step.LoadVar, step.VarFile, root)
}

// validateDoArtifactFlow walks a do: block's children, which are TRANSPARENT
// here — deliberately unlike the concurrent blocks blockBranches serves.
//
// A do:'s children run in sequence, so a later child may consume an earlier
// one's output exactly as two consecutive plan steps do. That is the very
// thing a concurrent block must forbid (branches have no order to consume
// along), which is why this cannot go through the shared branch walk: the
// children thread the SAME available map in declaration order, and what they
// produce stays visible to the steps after the block.
func validateDoArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, available map[string]bool) error {
	pre := maps.Clone(available)

	for childIndex := range step.Do {
		err := validateStepArtifactFlow(cfg, jobName, childIndex, step.Do[childIndex], available)
		if err != nil {
			return err
		}
	}

	return validateStepHooks(cfg, jobName, i, step, pre, maps.Clone(available))
}

// blockBranches returns a concurrent block's branches, or nil for an ordinary
// step.
func blockBranches(step config.Step) []config.Step {
	//kindswitch:ignore only the container kinds have branches; the leaf kinds are the point of the default
	switch {
	case step.InParallel != nil:
		return step.InParallel.Steps
	case step.Race != nil:
		return step.Race.Steps
	case step.Ensemble != nil:
		return step.Ensemble.Agents
	default:
		return nil
	}
}

// validatePutArtifactFlow checks a put step's declared inputs. A put produces
// no artifacts into the build store, so its pre and post hook views are the
// same one.
func validatePutArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, available map[string]bool) error {
	// inputs: all draws on whatever exists, so there is nothing to check.
	if !step.InputsAll() {
		err := checkInputsAvailable(jobName, i, "put", step.Put, step.InputNames(), available)
		if err != nil {
			return err
		}
	}

	// A put is artifact-neutral: it publishes, it does not fetch. A plan that
	// wants the just-published version as an artifact writes an explicit
	// get step after the put — there is no implicit get.
	pre := maps.Clone(available)

	return validateStepHooks(cfg, jobName, i, step, pre, maps.Clone(available))
}

// validateBlockArtifactFlow validates a concurrent block's branches and then
// the block's own hooks.
func validateBlockArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, branches []config.Step, available map[string]bool) error {
	pre := maps.Clone(available)

	err := validateParallelArtifactFlow(cfg, jobName, i, branches, available)
	if err != nil {
		return err
	}

	return validateStepHooks(cfg, jobName, i, step, pre, maps.Clone(available))
}

// validateParallelArtifactFlow checks each branch of an in_parallel: block
// against the artifacts available when the BLOCK started — never against each
// other's outputs.
//
// Concurrent branches have no order between them, so a branch consuming a
// sibling's output is a race, and the plan-time answer has to be "that
// artifact is not available here" rather than "sometimes". Everything the
// branches produce joins the view after the block, where a later step may
// legitimately consume it.
func validateParallelArtifactFlow(cfg *config.Config, jobName string, i int, branches []config.Step, available map[string]bool) error {
	start := maps.Clone(available)

	for _, branch := range branches {
		produced := maps.Clone(start)

		err := validateStepArtifactFlow(cfg, jobName, i, branch, produced)
		if err != nil {
			return err
		}

		for name := range produced {
			available[name] = true
		}
	}

	return nil
}

func validateAgentArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, available map[string]bool) error {
	pre := maps.Clone(available)

	err := checkInputsAvailable(jobName, i, "agent", step.Agent, step.InputNames(), available)
	if err != nil {
		return err
	}

	err = checkImageAvailable(cfg, jobName, i, "agent", step.Agent, step.Image, available)
	if err != nil {
		return err
	}

	// Ahead of the dir: check: an output exists, empty, before the step runs, so dir: may name one nothing earlier produced (as Concourse's run.dir may).
	for _, out := range step.Outputs {
		available[out] = true
	}

	// dir: names the artifact the step works in, so an agent pointed at a directory nothing fetched fails here.
	err = checkDirAvailable(jobName, i, "agent", step.Agent, step.Dir, available)
	if err != nil {
		return err
	}

	// ...and, since a step's directory is materialized from its own declared
	// inputs, dir: must also BE one of them (or a declared output) — an
	// available-but-undeclared artifact would flow-validate here and then be
	// absent from the materialized space at run time.
	err = checkDirDeclared(jobName, i, "agent", step)
	if err != nil {
		return err
	}

	err = checkMessageFileArtifactsAvailable(jobName, i, step.Agent, step.MessageFiles, step.InputNames(), available)
	if err != nil {
		return err
	}

	return validateStepHooks(cfg, jobName, i, step, pre, maps.Clone(available))
}

// checkMessageFileArtifactsAvailable validates a run-time message_files:
// {artifact, path}'s artifact (see config.FileRef.Deferred) against both the
// plan's available set (so it names something actually fetched/produced
// somewhere in the job) and the step's own declared inputs: (so the artifact
// is guaranteed to be materialized into the step's working directory by the
// time it reads it — see internal/agent's resolveDeferredPrompt, which reads
// relative to the step's own build space, not the whole plan). A load-time
// message_files: (a plain path, not a mapping) names no artifact and is a
// no-op here.
func checkMessageFileArtifactsAvailable(jobName string, i int, agentName string, messageFiles []*config.FileRef, inputs []string, available map[string]bool) error {
	for _, ref := range messageFiles {
		if !ref.Deferred() {
			continue
		}

		if !available[ref.Artifact] {
			return fmt.Errorf("job %q step %d (agent %q): message_files artifact %q is not a resource fetched or an output produced earlier in the plan",
				jobName, i, agentName, ref.Artifact)
		}

		if !slices.Contains(inputs, ref.Artifact) {
			return fmt.Errorf("job %q step %d (agent %q): message_files artifact %q must also be declared in this step's inputs",
				jobName, i, agentName, ref.Artifact)
		}
	}

	return nil
}

func validateTaskArtifactFlow(cfg *config.Config, jobName string, i int, step config.Step, available map[string]bool) error {
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		return fmt.Errorf("job %q step %d: %w", jobName, i, err)
	}

	pre := maps.Clone(available)

	// input_mapping/output_mapping rename a declared input/output onto the
	// plan-artifact name, so availability is checked — and outputs registered —
	// against the mapped name (the declared name is only what the task sees on
	// disk).
	err = checkInputsAvailable(jobName, i, "task", rt.Name, mapArtifacts(rt.Inputs, rt.InputMapping), available)
	if err != nil {
		return err
	}

	err = checkImageAvailable(cfg, jobName, i, "task", rt.Name, step.Image, available)
	if err != nil {
		return err
	}

	for _, out := range mapArtifacts(rt.Outputs, rt.OutputMapping) {
		available[out] = true
	}

	return validateStepHooks(cfg, jobName, i, step, pre, maps.Clone(available))
}

// validateStepHooks validates each of a step's hooks against the artifact view
// it will actually see at runtime: on_success against post (the step succeeded
// and captured its outputs), every failure-path hook against pre. A hook's own
// declared outputs are captured into the build store but never added to the
// plan's available set — a later plan step must not depend on a
// conditionally-run hook's output.
func validateStepHooks(cfg *config.Config, jobName string, i int, step config.Step, pre, post map[string]bool) error {
	return step.Hooks.Each(func(name string, hook *config.Step) error { //nolint:wrapcheck // callback errors carry full job/step/hook context
		view := pre
		if name == "on_success" {
			view = post
		}

		return validateHookArtifactFlow(cfg, jobName, i, name, *hook, view)
	})
}

// validateHookArtifactFlow checks one hook step's resolved inputs against the
// view available to it, then recurses into the hook's own nested hooks (which
// see the same view). Hook outputs are intentionally not folded into the view.
func validateHookArtifactFlow(cfg *config.Config, jobName string, i int, hookName string, hook config.Step, view map[string]bool) error {
	var (
		inputs     []string
		kind, name string
	)

	//kindswitch:ignore get: is the one kind rejected in a hook body at load time (see config's validateHookStep)
	switch {
	case hook.Task != "":
		rt, err := cfg.ResolveTask(hook)
		if err != nil {
			return fmt.Errorf("job %q step %d %s hook: %w", jobName, i, hookName, err)
		}

		inputs, kind, name = rt.Inputs, "task", rt.Name
	case hook.Put != "":
		inputs, kind, name = hook.InputNames(), "put", hook.Put
	case hook.Agent != "":
		inputs, kind, name = hook.InputNames(), "agent", hook.Agent
	case hook.Try != nil:
		// try: is a legal hook body, and a wrapper declares no inputs of its
		// own — the step it wraps does, against this same view. Falling into
		// the no-case branch left those inputs entirely unchecked, which is
		// the whole failure mode this switch's kind coverage is about.
		err := validateHookArtifactFlow(cfg, jobName, i, hookName+" (try)", *hook.Try, view)
		if err != nil {
			return err
		}
	}

	err := checkHookInputsAvailable(cfg, jobName, i, hookName, kind, name, inputs, hook.Image, view)
	if err != nil {
		return err
	}

	err = checkHookAgentArtifacts(jobName, i, hookName, hook, inputs, view)
	if err != nil {
		return err
	}

	return hook.Hooks.Each(func(nestedName string, nested *config.Step) error { //nolint:wrapcheck // callback errors carry full job/step/hook context
		return validateHookArtifactFlow(cfg, jobName, i, hookName+"."+nestedName, *nested, view)
	})
}

// checkHookInputsAvailable holds a hook's inputs and artifact image to its
// view; split out of validateHookArtifactFlow for the cyclomatic budget.
func checkHookInputsAvailable(cfg *config.Config, jobName string, i int, hookName, kind, name string, inputs []string, image string, view map[string]bool) error {
	for _, in := range inputs {
		if !view[in] {
			return fmt.Errorf("job %q step %d %s hook (%s %q): input %q is not available to this hook",
				jobName, i, hookName, kind, name, in)
		}
	}

	return checkImageAvailable(cfg, jobName, i, hookName+" hook "+kind, name, image, view)
}

// checkHookAgentArtifacts holds an agent hook to a plan agent's dir: and message_files: rules against the hook's view; split out of validateHookArtifactFlow for the cyclomatic budget.
func checkHookAgentArtifacts(jobName string, i int, hookName string, hook config.Step, inputs []string, view map[string]bool) error {
	if hook.Agent == "" {
		return nil
	}

	own := map[string]bool{}
	maps.Copy(own, view)

	for _, out := range hook.Outputs {
		own[out] = true
	}

	kind := hookName + " hook agent"

	err := checkDirAvailable(jobName, i, kind, hook.Agent, hook.Dir, own)
	if err != nil {
		return err
	}

	err = checkDirDeclared(jobName, i, kind, hook)
	if err != nil {
		return err
	}

	for _, ref := range hook.MessageFiles {
		if !ref.Deferred() {
			continue
		}

		if !view[ref.Artifact] {
			return fmt.Errorf("job %q step %d %s hook (agent %q): message_files artifact %q is not available to this hook",
				jobName, i, hookName, hook.Agent, ref.Artifact)
		}

		if !slices.Contains(inputs, ref.Artifact) {
			return fmt.Errorf("job %q step %d %s hook (agent %q): message_files artifact %q must also be declared in this hook's inputs",
				jobName, i, hookName, hook.Agent, ref.Artifact)
		}
	}

	return nil
}

// checkImageAvailable holds an image: naming an artifact to the same rule an
// input is held to: something earlier in the plan must have fetched it. The
// image need not ALSO be an input, as in Concourse — the daemon pulls it, so
// the artifact's files are never read. An earlier task's output of the same
// name does not count: only a get's version names an image.
func checkImageAvailable(cfg *config.Config, jobName string, i int, kind, name, image string, available map[string]bool) error {
	if image == "" || available[fetchedByGet(image)] || !cfg.ImageArtifacts(jobName)[image] {
		return nil
	}

	return fmt.Errorf("job %q step %d (%s %q): image %q names an artifact no earlier get in the plan fetches: get it first, or rename the resource if you meant the image %s",
		jobName, i, kind, name, image, image)
}

// fetchedByGet is the key the flow walk marks a get's own name under, beside
// the artifact name an output may also claim. ':' is outside the artifact
// name pattern, so no input or output can collide with it.
func fetchedByGet(name string) string {
	return "get:" + name
}

func checkInputsAvailable(jobName string, i int, kind, name string, inputs []string, available map[string]bool) error {
	for _, in := range inputs {
		if !available[in] {
			return fmt.Errorf("job %q step %d (%s %q): input %q is not a resource fetched or an output produced earlier in the plan",
				jobName, i, kind, name, in)
		}
	}

	return nil
}

// checkAcrossFromFileAvailable validates that every from_file: axis names an
// available artifact by its first path component, exactly as dir: does — the
// runner reads the file by materializing that artifact (see internal/pipeline's
// readAcrossFile), so an axis pointing at something nothing produces would fail
// mid-run, after the steps before it had already been paid for.
func checkAcrossFromFileAvailable(jobName string, i int, step config.Step, available map[string]bool) error {
	for _, axis := range step.Across {
		if !axis.Runtime() {
			continue
		}

		root := axis.SourceArtifact()
		if !available[root] {
			return fmt.Errorf("job %q step %d: across var %q reads %q, whose artifact %q is not a resource fetched or an output produced earlier in the plan",
				jobName, i, axis.Var, axis.FromFile, root)
		}
	}

	return nil
}

// checkDirDeclared validates that an agent step's dir: artifact (its first
// path component) is one of the step's own declared inputs: or outputs: —
// only declared artifacts are materialized into the step's directory, so an
// undeclared dir: would be a missing directory at run time.
func checkDirDeclared(jobName string, i int, kind string, step config.Step) error {
	root := firstPathComponent(step.Dir)
	if root == "" || root == "." {
		return nil
	}

	for _, name := range step.InputNames() {
		if name == root {
			return nil
		}
	}

	for _, name := range step.Outputs {
		if name == root {
			return nil
		}
	}

	return fmt.Errorf("job %q step %d (%s %q): dir %q names artifact %q, which the step does not declare in inputs: or outputs: — only declared artifacts are materialized into its working directory",
		jobName, i, kind, step.Agent, step.Dir, root)
}

// checkDirAvailable validates that an agent step's dir:, when set, names an
// available artifact by its first path component (so dir: repo/cmd requires
// repo). An empty or "." dir is the workspace root and names nothing.
func checkDirAvailable(jobName string, i int, kind, name, dir string, available map[string]bool) error {
	if dir == "" {
		return nil
	}

	root := firstPathComponent(dir)
	if root == "" || root == "." {
		return nil
	}

	if !available[root] {
		return fmt.Errorf("job %q step %d (%s %q): dir %q names %q, which is not a resource fetched or an output produced earlier in the plan",
			jobName, i, kind, name, dir, root)
	}

	return nil
}

// mapArtifacts renames each declared name through mapping (task-config name ->
// plan-artifact name), leaving unmapped names untouched. Used to resolve a
// task step's inputs/outputs onto the plan-artifact names its input_mapping/
// output_mapping point at.
func mapArtifacts(names []string, mapping map[string]string) []string {
	if len(mapping) == 0 {
		return names
	}

	out := make([]string, len(names))

	for i, name := range names {
		if mapped, ok := mapping[name]; ok {
			out[i] = mapped
		} else {
			out[i] = name
		}
	}

	return out
}

// firstPathComponent returns the first path segment of a cleaned relative
// path (e.g. "repo" for "repo/cmd", "repo" for "repo").
func firstPathComponent(dir string) string {
	cleaned := filepath.Clean(dir)

	for {
		parent := filepath.Dir(cleaned)
		if parent == "." || parent == cleaned {
			return cleaned
		}

		cleaned = parent
	}
}
