package config

import (
	"cmp"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// secretRefPattern finds anything shaped like a reference; whether its name is legal is SecretRef's to say, so a malformed one is refused rather than passed through as text.
var secretRefPattern = regexp.MustCompile(`\(\(([a-z0-9_-]+):([^()]*)\)\)`)

var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

var shellIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SecretRef is a ((source:name)) reference: the source is the deployment's, the name the pipeline's.
type SecretRef struct {
	Source string
	Name   string
}

func (r SecretRef) String() string { return "((" + r.Source + ":" + r.Name + "))" }

// ParseSecretRef reads a value that must be one reference and nothing else.
func ParseSecretRef(value string) (SecretRef, error) {
	match := secretRefPattern.FindStringSubmatch(value)
	if match == nil || match[0] != value {
		return SecretRef{}, fmt.Errorf("%q must be exactly one ((source:name)) reference", value)
	}

	ref := SecretRef{Source: match[1], Name: match[2]}

	// No empty, . or .. segment, so ((ci:../prod/db)) cannot climb out of the pipeline's folder in a lookup path.
	if !secretNamePattern.MatchString(ref.Name) || slices.ContainsFunc(strings.Split(ref.Name, "/"), func(segment string) bool {
		return segment == "" || segment == "." || segment == ".."
	}) {
		return SecretRef{}, fmt.Errorf("%s: a secret name is letters, digits and _ . / -, with no empty, . or .. segment", ref)
	}

	return ref, nil
}

// HasSecretRef reports text shaped like a reference anywhere in it.
func HasSecretRef(text string) bool { return secretRefPattern.MatchString(text) }

// SecretRefs is every reference the pipeline makes, in a stable order.
func (c *Config) SecretRefs() []SecretRef {
	seen := map[SecretRef]bool{}

	add := func(secrets map[string]string) {
		for _, value := range secrets {
			ref, err := ParseSecretRef(value)
			if err == nil {
				seen[ref] = true
			}
		}
	}

	for _, task := range c.Tasks {
		add(task.Secrets)
	}

	for _, agent := range c.Agents {
		add(agent.Secrets)
	}

	for _, r := range c.Resources {
		add(r.Secrets)
	}

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(_ string, step *Step) error {
			add(step.Secrets)

			return nil
		})
	}

	return slices.SortedFunc(maps.Keys(seen), func(a, b SecretRef) int { return strings.Compare(a.String(), b.String()) })
}

// validateSecrets runs on the PARSED config, never on rendered strings: a resource version or webhook body holding ((ci:prod)) must not make steps fetch a secret nobody wrote.
func (c *Config) validateSecrets() error {
	for _, task := range c.Tasks {
		err := checkSecretsMap(fmt.Sprintf("task %q", task.Name), task.Secrets, task.Env)
		if err != nil {
			return err
		}
	}

	err := c.validateAgentSecrets()
	if err != nil {
		return err
	}

	for _, r := range c.Resources {
		env := r.Env

		// The type's env: names reach the same commands, so a collision there leaves merge order to pick a value too.
		resourceType, findErr := c.FindResourceType(r.Type)
		if findErr == nil && resourceType != nil {
			env = UnionEnv(resourceType.Env, r.Env)
		}

		err := checkSecretsMap(fmt.Sprintf("resource %q", r.Name), r.Secrets, env)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(c.checkStepSecrets)
		if err != nil {
			return err
		}
	}

	return refuseMisplacedSecrets("", reflect.ValueOf(c).Elem())
}

func (c *Config) checkStepSecrets(label string, step *Step) error {
	// On what the step RUNS with: its own env: or secrets: replaces its entry's, so a collision can be across the two levels.
	env, secrets := c.effectiveEnvAndSecrets(step)
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		if slices.Contains(env, name) {
			return fmt.Errorf("%s: %s is named in both env and secrets (between the step and its entry)", label, name)
		}
	}

	if step.Secrets == nil {
		return nil
	}

	// A get or put step carries no secrets: map of its own; its resource's applies, so one written there would be silently ignored rather than honoured.
	kind, _ := step.Kind()
	if kind != StepKindTask && kind != StepKindAgent {
		return fmt.Errorf("%s: secrets: is valid on task and agent steps only", label)
	}

	if kind == StepKindAgent && c.agentIsCLI(step.Agent) {
		return fmt.Errorf("%s: secrets: on a cli agent — the CLI runs its own tools and shows their output to its model, which steps cannot scrub", label)
	}

	return checkSecretsMap(label, step.Secrets, step.Env)
}

func (c *Config) agentIsCLI(name string) bool {
	agent, err := c.FindAgent(name)

	return err == nil && agent != nil && agentUsesCLI(*agent)
}

// validateAgentSecrets refuses every place an agent's secrets would be silently dropped: only an agent step resolves them.
func (c *Config) validateAgentSecrets() error {
	for _, agent := range c.Agents {
		err := checkAgentSecrets(agent)
		if err != nil {
			return err
		}
	}

	withSecrets := func(name string) bool {
		agent, err := c.FindAgent(name)

		return err == nil && agent != nil && len(agent.Secrets) > 0
	}

	err := c.refuseSecretSubAgents(withSecrets)
	if err != nil {
		return err
	}

	for _, fix := range c.fixSpecs() {
		if fix != nil && withSecrets(fix.Agent) {
			return fmt.Errorf("fix: names agent %q, which has secrets:; a fix agent never receives secrets", fix.Agent)
		}
	}

	return nil
}

func checkAgentSecrets(agent Agent) error {
	if len(agent.Secrets) == 0 {
		return nil
	}

	if agentUsesCLI(agent) {
		return fmt.Errorf("agent %q: secrets: on a cli agent — the CLI runs its own tools and shows their output to its model, which steps cannot scrub", agent.Name)
	}

	return checkSecretsMap(fmt.Sprintf("agent %q", agent.Name), agent.Secrets, agent.Env)
}

func (c *Config) refuseSecretSubAgents(withSecrets func(string) bool) error {
	for _, agent := range c.Agents {
		for _, spec := range agent.Tools {
			if spec.Agent != "" && withSecrets(spec.Agent) {
				return fmt.Errorf("agent %q: tool %q delegates to agent %q, which has secrets:; only an agent step resolves them", agent.Name, ToolSpecName(spec), spec.Agent)
			}
		}
	}

	return nil
}

// fixSpecs is every fix: in the pipeline, on tasks: entries and on steps.
func (c *Config) fixSpecs() []*FixSpec {
	fixes := []*FixSpec{}
	for _, task := range c.Tasks {
		fixes = append(fixes, task.Fix)
	}

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(_ string, step *Step) error {
			fixes = append(fixes, step.Fix)

			return nil
		})
	}

	return fixes
}

func checkSecretsMap(context string, secrets map[string]string, env []string) error {
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		err := checkEnvName(context+" secrets", name)
		if err != nil {
			return err
		}

		// The name is exported by a shell on a bare worker (`export NAME=…`), which rejects anything but an identifier, and TRACEPARENT is overwritten whenever tracing is on: either way the step would silently run without its credential.
		if !shellIdentifier.MatchString(name) || name == "TRACEPARENT" {
			return fmt.Errorf("%s: secrets key %q must be a shell variable name (letters, digits, _) and not TRACEPARENT", context, name)
		}

		// A name in both would leave merge order to decide which value the step sees.
		if slices.Contains(env, name) {
			return fmt.Errorf("%s: %s is named in both env and secrets", context, name)
		}

		_, err = ParseSecretRef(secrets[name])
		if err != nil {
			return fmt.Errorf("%s: secrets.%s: %w", context, name, err)
		}
	}

	return nil
}

// refuseMisplacedSecrets walks every yaml-reachable string; a field tagged secret:"ref" is the only home a reference has, everywhere else it would be hashed, stored and rendered into a command.
func refuseMisplacedSecrets(path string, value reflect.Value) error {
	switch value.Kind() { //nolint:exhaustive // only containers and strings can hold text
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil
		}

		return refuseMisplacedSecrets(path, value.Elem())
	case reflect.String:
		if match := secretRefPattern.FindString(value.String()); match != "" {
			return fmt.Errorf("%s: %s is a secret reference, which belongs only in a secrets map", strings.TrimPrefix(path, "."), match)
		}
	case reflect.Slice, reflect.Array:
		return refuseMisplacedInList(path, value)
	case reflect.Map:
		return refuseMisplacedInMap(path, value)
	case reflect.Struct:
		return refuseMisplacedInStruct(path, value)
	}

	return nil
}

func refuseMisplacedInList(path string, value reflect.Value) error {
	for i := range value.Len() {
		err := refuseMisplacedSecrets(path+"["+strconv.Itoa(i)+"]", value.Index(i))
		if err != nil {
			return err
		}
	}

	return nil
}

func refuseMisplacedInMap(path string, value reflect.Value) error {
	iter := value.MapRange()
	for iter.Next() {
		err := refuseMisplacedSecrets(path+"."+fmt.Sprint(iter.Key().Interface()), iter.Value())
		if err != nil {
			return err
		}
	}

	return nil
}

func refuseMisplacedInStruct(path string, value reflect.Value) error {
	for i := range value.NumField() {
		field := value.Type().Field(i)

		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if !field.IsExported() || name == "-" || field.Tag.Get("secret") == "ref" {
			continue
		}

		if name == "" {
			name = field.Name
		}

		err := refuseMisplacedSecrets(path+"."+name, value.Field(i))
		if err != nil {
			return err
		}
	}

	return nil
}

// effectiveEnvAndSecrets is the env: and secrets: a task or agent step runs with, each declared-wins over its entry's.
func (c *Config) effectiveEnvAndSecrets(step *Step) ([]string, map[string]string) {
	var env []string

	var secrets map[string]string

	task, taskErr := c.FindTask(step.Task)
	if step.Run == "" && step.Task != "" && taskErr == nil && task != nil {
		env, secrets = task.Env, task.Secrets
	}

	agent, agentErr := c.FindAgent(step.Agent)
	if step.Agent != "" && agentErr == nil && agent != nil {
		env, secrets = agent.Env, agent.Secrets
	}

	if step.Env != nil {
		env = step.Env
	}

	if step.Secrets != nil {
		secrets = step.Secrets
	}

	return env, secrets
}

// SecretUse is one place a run or poll resolves a reference from. The same reference resolves separately for each, because a lookup path's template and when: see the job and resource names.
type SecretUse struct {
	Ref           SecretRef
	Job, Resource string
	// Polled is a trigger: true resource's check outside any run, where there is no job name to scope on.
	Polled bool
}

// SecretUses is every (reference, where) pair the pipeline would resolve, deduplicated and in a stable order: what `steps validate --live` resolves in the daemon's stead.
func (c *Config) SecretUses() []SecretUse {
	seen := map[SecretUse]bool{}

	add := func(secrets map[string]string, use SecretUse) {
		for _, value := range secrets {
			ref, err := ParseSecretRef(value)
			if err == nil {
				use.Ref = ref
				seen[use] = true
			}
		}
	}

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(_ string, step *Step) error {
			switch kind, _ := step.Kind(); kind {
			case StepKindTask, StepKindAgent:
				_, secrets := c.effectiveEnvAndSecrets(step)
				add(secrets, SecretUse{Job: job.Name})
			case StepKindGet, StepKindPut:
				// The resolved name, since a get: or put: may alias its resource; stepIsPolled, since a passed: get is checked by the poller as a trigger: true one is.
				r, err := c.FindResource(cmp.Or(step.GetResourceName(), step.PutResourceName()))
				if err == nil && r != nil {
					add(r.Secrets, SecretUse{Job: job.Name, Resource: r.Name})

					if stepIsPolled(*step) {
						add(r.Secrets, SecretUse{Resource: r.Name, Polled: true})
					}
				}
			case StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindDo:
				// A block's members are visited on their own; the rest carry no secrets: map.
			}

			return nil
		})
	}

	return slices.SortedFunc(maps.Keys(seen), func(a, b SecretUse) int {
		return cmp.Or(
			strings.Compare(a.Ref.String(), b.Ref.String()),
			strings.Compare(a.Resource, b.Resource),
			strings.Compare(a.Job, b.Job),
		)
	})
}
