package config

// ((var)) interpolation and the load_var: step.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// varPattern matches a ((name)) reference. Concourse's spelling, kept because
// a pipeline moving between the two should not have to be rewritten.
var varPattern = regexp.MustCompile(`\(\(([a-zA-Z0-9_.\-]+)\)\)`) //nolint:gochecknoglobals // compiled once, read-only

// InterpolateVars substitutes ((name)) throughout a pipeline's YAML source,
// before it is parsed.
//
// Textual, not structural, and deliberately so: a var may appear anywhere a
// value does — inside a URI, in the middle of a command, as a whole mapping
// value — and a structural pass would have to enumerate every field that might
// contain one, which is the same list that goes stale every time a field is
// added.
//
// Unknown references are LEFT IN PLACE rather than blanked. They are either a
// load_var: value that does not exist yet or a mistake, and blanking them
// would turn both into a silently empty string in a command.
func InterpolateVars(source []byte, vars map[string]string) []byte {
	if len(vars) == 0 {
		return source
	}

	return varPattern.ReplaceAllFunc(source, func(match []byte) []byte {
		name := varPattern.FindSubmatch(match)[1]

		value, ok := vars[string(name)]
		if !ok {
			return match
		}

		return []byte(value)
	})
}

// UnresolvedVars lists every ((name)) still present in a value, sorted.
func UnresolvedVars(value string) []string {
	matches := varPattern.FindAllStringSubmatch(value, -1)

	seen := map[string]bool{}
	names := make([]string, 0, len(matches))

	for _, match := range matches {
		if !seen[match[1]] {
			seen[match[1]] = true
			names = append(names, match[1])
		}
	}

	sort.Strings(names)

	return names
}

// RenderVars substitutes ((name)) in a single value from a run-time var map,
// leaving unknown references in place.
func RenderVars(value string, vars map[string]string) string {
	if value == "" || !strings.Contains(value, "((") {
		return value
	}

	return string(InterpolateVars([]byte(value), vars))
}

// validateVars rejects a ((name)) that nothing will ever supply, and a
// malformed load_var: step.
//
// Catching it at load is the whole point: an unresolved var otherwise reaches
// a shell command as the literal text `((repo_uri))`, which fails somewhere
// far from the mistake.
//
// A var counts as produced only where the run GUARANTEES it was captured,
// because anywhere else the literal text is exactly what arrives:
//   - sequential steps (the plan, a do: block) see what ran before them;
//   - concurrent branches see only what preceded the block, since whether one
//     branch sees another's capture is a race; after the block, an
//     in_parallel: (which succeeds only when every branch did) yields every
//     branch's captures, while a race: or ensemble: yields only those common
//     to all branches, since any one of them may be the one that won;
//   - a try: body's captures stay inside it, since the try succeeds whether or
//     not the body did;
//   - hooks see what preceded the step, and on_success also what the step
//     captured; nothing a hook captures escapes it.
func (c *Config) validateVars() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(validateLoadVarStep)
		if err != nil {
			return err
		}

		produced := varSet{}

		for i := range job.Plan {
			produced, err = checkVarFlow(fmt.Sprintf("%s step %d%s", job.label(), i, job.Plan[i].at()), &job.Plan[i], produced)
			if err != nil {
				return err
			}
		}

		err = checkHookFlow(job.Hooks, varSet{}, produced, func(name string) string {
			return fmt.Sprintf("%s %s hook", job.label(), name)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

type varSet map[string]bool

func (s varSet) with(names ...string) varSet {
	out := make(varSet, len(s)+len(names))
	for name := range s {
		out[name] = true
	}

	for _, name := range names {
		out[name] = true
	}

	return out
}

// checkVarFlow checks one step and everything nested in it against the vars
// available when it starts, and returns those available once it has
// succeeded. Labels follow walkStepTree's, so an error names the same place
// every other validator would.
func checkVarFlow(label string, step *Step, before varSet) (varSet, error) {
	err := checkStepVars(label, step, before)
	if err != nil {
		return nil, err
	}

	after := before
	if step.LoadVar != "" {
		after = after.with(step.LoadVar)
	}

	if step.Try != nil {
		_, err = checkVarFlow(label+" (try)", step.Try, before)
		if err != nil {
			return nil, err
		}
	}

	after, err = checkBranchFlow(label, step, before, after)
	if err != nil {
		return nil, err
	}

	for i := range step.Do {
		after, err = checkVarFlow(fmt.Sprintf("%s (do step %d)", label, i), &step.Do[i], after)
		if err != nil {
			return nil, err
		}
	}

	err = checkHookFlow(step.Hooks, before, after, func(name string) string {
		return fmt.Sprintf("%s (%s hook)", label, name)
	})
	if err != nil {
		return nil, err
	}

	return after, nil
}

// checkBranchFlow checks a concurrent block's branches, each from what preceded
// the block, and adds what the block yields once it has succeeded to after.
func checkBranchFlow(label string, step *Step, before, after varSet) (varSet, error) {
	for kind, branches := range branchesOf(step) {
		var joined varSet

		for i := range branches {
			out, err := checkVarFlow(fmt.Sprintf("%s (%s branch %d)", label, kind, i), &branches[i], before)
			if err != nil {
				return nil, err
			}

			joined = joinBranch(kind, joined, out, i == 0)
		}

		for name := range joined {
			after = after.with(name)
		}
	}

	return after, nil
}

// checkHookFlow checks hooks against what preceded their step, giving
// on_success alone what the step captured.
func checkHookFlow(hooks Hooks, before, after varSet, label func(name string) string) error {
	return hooks.Each(func(name string, hook *Step) error {
		seen := before
		if name == "on_success" {
			seen = after
		}

		_, err := checkVarFlow(label(name), hook, seen)

		return err
	})
}

// joinBranch folds one branch's captures into what its block yields: all of
// them for in_parallel:, only the common ones for a block whose outcome is
// whichever branch won.
func joinBranch(kind string, joined, branch varSet, first bool) varSet {
	if first {
		return branch
	}

	if kind == "in_parallel" {
		for name := range joined {
			branch = branch.with(name)
		}

		return branch
	}

	common := varSet{}

	for name := range joined {
		if branch[name] {
			common[name] = true
		}
	}

	return common
}

// validateLoadVarStep checks a load_var: step names a var and a file.
func validateLoadVarStep(label string, step *Step) error {
	if step.LoadVar == "" {
		if step.VarFile != "" {
			return fmt.Errorf("%s: file: without load_var: has no meaning here", label)
		}

		return nil
	}

	if step.VarFile == "" {
		return fmt.Errorf("%s: load_var %q needs a file: to read the value from", label, step.LoadVar)
	}

	// The read happens on the orchestrator host, so a path leaving the step's
	// workspace names a file the pipeline does not own. One still carrying a
	// ((var)) is checked by the run once rendered.
	if len(UnresolvedVars(step.VarFile)) > 0 {
		return nil
	}

	if _, ok := ConfinedPath(".", step.VarFile); !ok {
		return fmt.Errorf("%s: load_var %q file %q escapes the workspace; it must be a path inside a declared input, like meta/version.txt", label, step.LoadVar, step.VarFile)
	}

	return nil
}

// checkStepVars rejects references a step makes to vars nothing supplies.
//
// The field list must match what renderStepVars actually substitutes, or a
// reference passes validation and then reaches the outside world verbatim: a
// `((version))` in a put's params: was neither rejected here nor rendered
// there, so the resource's out: published a release literally named
// "((version))".
func checkStepVars(label string, step *Step, produced map[string]bool) error {
	for _, field := range varFields(step) {
		for _, name := range UnresolvedVars(field.value) {
			if !produced[name] {
				return fmt.Errorf("%s: %s references ((%s)), which is neither passed with --var/--vars-file nor produced by an earlier load_var: step",
					label, field.name, name)
			}
		}
	}

	// params: is a free-form mapping rather than a field, so it is walked
	// rather than listed.
	for _, name := range unresolvedInValue(step.Params) {
		if !produced[name] {
			return fmt.Errorf("%s: params references ((%s)), which is neither passed with --var/--vars-file nor produced by an earlier load_var: step",
				label, name)
		}
	}

	return nil
}

// varField is one string field a var may appear in.
type varField struct{ name, value string }

// VarFields lists a step's substitutable string fields, so validation and
// run-time rendering cannot drift apart. Adding a field here is what makes it
// both checked and rendered.
func varFields(step *Step) []varField {
	fields := make([]varField, 0, 4+len(step.Messages))
	fields = append(fields,
		varField{"run", step.Run},
		varField{"image", step.Image},
		varField{"dir", step.Dir},
		varField{"file", step.VarFile},
	)

	for i, message := range step.Messages {
		fields = append(fields, varField{fmt.Sprintf("messages[%d]", i), message})
	}

	return fields
}

// unresolvedInValue walks a decoded YAML value — a params: mapping, a nested
// list, a scalar — and returns every ((name)) still in it.
func unresolvedInValue(value any) []string {
	var names []string

	switch typed := value.(type) {
	case string:
		names = append(names, UnresolvedVars(typed)...)
	case map[string]any:
		for _, nested := range typed {
			names = append(names, unresolvedInValue(nested)...)
		}
	case []any:
		for _, nested := range typed {
			names = append(names, unresolvedInValue(nested)...)
		}
	}

	return names
}

// RenderValue substitutes ((name)) throughout a decoded YAML value, returning
// a copy. Used for params:, which is a free-form mapping rather than a field.
func RenderValue(value any, vars map[string]string) any {
	switch typed := value.(type) {
	case string:
		return RenderVars(typed, vars)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			out[key] = RenderValue(nested, vars)
		}

		return out
	case []any:
		out := make([]any, len(typed))
		for i, nested := range typed {
			out[i] = RenderValue(nested, vars)
		}

		return out
	default:
		return value
	}
}
