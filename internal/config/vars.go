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
func (c *Config) validateVars() error {
	for _, job := range c.Jobs {
		produced := map[string]bool{}

		for i := range job.Plan {
			step := &job.Plan[i]
			label := fmt.Sprintf("job %q step %d", job.Name, i)

			err := validateLoadVarStep(label, step)
			if err != nil {
				return err
			}

			err = checkStepVars(label, step, produced)
			if err != nil {
				return err
			}

			if step.LoadVar != "" {
				produced[step.LoadVar] = true
			}
		}
	}

	return nil
}

// validateLoadVarStep checks a load_var: step names a var and a file.
func validateLoadVarStep(label string, step *Step) error {
	if step.LoadVar == "" {
		if step.VarFile != "" && step.Task == "" && step.Get == "" {
			return fmt.Errorf("%s: file: without load_var: has no meaning here", label)
		}

		return nil
	}

	if step.VarFile == "" {
		return fmt.Errorf("%s: load_var %q needs a file: to read the value from", label, step.LoadVar)
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
	return []varField{
		{"run", step.Run},
		{"prompt", step.Prompt},
		{"image", step.Image},
		{"dir", step.Dir},
		{"file", step.VarFile},
	}
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
