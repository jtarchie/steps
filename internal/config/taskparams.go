package config

// A task's params: — values handed to its commands as environment variables, Concourse's task params.

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

//nolint:gochecknoglobals // compiled once, read-only
var paramNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// mergeParams is base with each of over's names replacing base's; nil when both are.
func mergeParams(base, over map[string]any) map[string]any {
	if base == nil && over == nil {
		return nil
	}

	merged := make(map[string]any, len(base)+len(over))
	maps.Copy(merged, base)
	maps.Copy(merged, over)

	return merged
}

// paramEnv is params as the environment a command sees: a string as written, null unset, anything else as JSON, as Concourse passes them.
func paramEnv(params map[string]any) map[string]string {
	if len(params) == 0 {
		return nil
	}

	env := make(map[string]string, len(params))

	for name, value := range params {
		switch v := value.(type) {
		case nil:
			continue
		case string:
			env[name] = v
		default:
			encoded, err := json.Marshal(v)
			if err != nil {
				// A YAML-decoded value always encodes; %v is only the fallback a hand-built config could reach.
				env[name] = fmt.Sprint(v)

				continue
			}

			env[name] = string(encoded)
		}
	}

	return env
}

// validateTaskParams checks every tasks: entry's declared names, then every task step's: a step may set only what its entry declares, and no name may also come from env:, secrets: or steps itself.
func (c *Config) validateTaskParams() error {
	for i := range c.Tasks {
		task := &c.Tasks[i]

		err := checkParamNames(fmt.Sprintf("task %q", task.Name), task.Params, task.Env, task.Secrets)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(c.checkStepParams)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) checkStepParams(label string, step *Step) error {
	if step.Task == "" || step.Params == nil {
		return nil
	}

	entry := c.taskEntry(*step)
	if entry != nil {
		for _, name := range slices.Sorted(maps.Keys(step.Params)) {
			if _, declared := entry.Params[name]; !declared {
				return fmt.Errorf("%s: params.%s: task %q declares no such param (it declares %s)", label, name, entry.Name, declaredParams(entry.Params))
			}
		}
	}

	rt, err := c.ResolveTask(*step)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable task is validateStepReferences' to report
	}

	return checkParamNames(label, step.Params, rt.Env, rt.Secrets)
}

func checkParamNames(label string, params map[string]any, env []string, secrets map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(params)) {
		switch {
		case !paramNamePattern.MatchString(name):
			return fmt.Errorf("%s: params.%s: not a variable name ([A-Za-z_][A-Za-z0-9_]*)", label, name)
		case slices.Contains(reservedEnvNames, name) || strings.HasPrefix(name, "STEPS_PENDING_"):
			return fmt.Errorf("%s: params.%s: set by steps for every step (build metadata)", label, name)
		case slices.Contains(env, name):
			return fmt.Errorf("%s: params.%s: also named in env:; one name, one source", label, name)
		}

		if _, isSecret := secrets[name]; isSecret {
			return fmt.Errorf("%s: params.%s: also named in secrets:; one name, one source", label, name)
		}
	}

	return nil
}

func declaredParams(params map[string]any) string {
	if len(params) == 0 {
		return "none"
	}

	return strings.Join(slices.Sorted(maps.Keys(params)), ", ")
}
