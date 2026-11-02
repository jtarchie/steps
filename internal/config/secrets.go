package config

import (
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

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Secrets == nil {
				return nil
			}

			// ponytail: task steps only; agent and resource secrets are later rungs of #98, refused rather than silently ignored until then.
			if kind, _ := step.Kind(); kind != StepKindTask {
				return fmt.Errorf("%s: secrets: is valid on task steps only", label)
			}

			return checkSecretsMap(label, step.Secrets, step.Env)
		})
		if err != nil {
			return err
		}
	}

	return refuseMisplacedSecrets("", reflect.ValueOf(c).Elem())
}

func checkSecretsMap(context string, secrets map[string]string, env []string) error {
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		err := checkEnvName(context+" secrets", name)
		if err != nil {
			return err
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
