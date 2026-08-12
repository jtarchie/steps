package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/internal/config"
)

// loadSchema compiles steps.schema.json.
func loadSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	data, err := os.ReadFile("steps.schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}

	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	compiler := jsonschema.NewCompiler()

	err = compiler.AddResource("steps.schema.json", doc)
	if err != nil {
		t.Fatalf("add schema resource: %v", err)
	}

	schema, err := compiler.Compile("steps.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	return schema
}

// yamlAsJSONValue reads a YAML file into the plain any-typed value the schema
// validator expects.
func yamlAsJSONValue(t *testing.T, path string) any {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path comes from this test's own glob over the repo
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var doc any

	err = yaml.Unmarshal(data, &doc)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// Round-trip through JSON so map keys are strings, as the validator wants.
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}

	var value any

	err = json.Unmarshal(encoded, &value)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	return value
}

// Every shipped example validates against the published schema. This is what
// keeps the schema honest: it is hand-written, so nothing but a test stops it
// drifting from what the loader actually accepts.
//
// The glob is deliberately non-recursive: examples/invalid/ is deliberately
// schema-invalid (see TestExamplesInvalid) and must stay out of it.
func TestExamplesMatchSchema(t *testing.T) {
	schema := loadSchema(t)

	matches, err := filepath.Glob("examples/*.yml")
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) == 0 {
		t.Fatal("no examples found")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			err := schema.Validate(yamlAsJSONValue(t, path))
			if err != nil {
				t.Errorf("%s does not match steps.schema.json:\n%v", path, err)
			}
		})
	}
}

// The schema rejects what the loader rejects. A schema that accepts everything
// would validate cleanly and be worth nothing.
func TestSchemaRejectsInvalidPipelines(t *testing.T) {
	schema := loadSchema(t)

	tests := []struct {
		name     string
		pipeline string
	}{
		{
			name:     "unknown top-level key",
			pipeline: "ressources: []\njobs: [{name: j, plan: [{task: t, run: 'true'}]}]\n",
		},
		{
			name:     "unknown step key",
			pipeline: "jobs: [{name: j, plan: [{task: t, run: 'true', promt: x}]}]\n",
		},
		{
			name:     "misspelled hook",
			pipeline: "jobs: [{name: j, plan: [{task: t, run: 'true', on_fail: {task: c, run: 'true'}}]}]\n",
		},
		{
			name:     "job without a plan",
			pipeline: "jobs: [{name: j}]\n",
		},
		{
			name:     "unknown workspace strategy",
			pipeline: "workspace: {strategy: rsync}\njobs: [{name: j, plan: []}]\n",
		},
		{
			name:     "custom tool without a run",
			pipeline: "agents: [{name: a, tools: [{name: build}]}]\njobs: [{name: j, plan: []}]\n",
		},
		{
			name:     "inputs scalar that is not all",
			pipeline: "jobs: [{name: j, plan: [{task: t, run: 'true', inputs: repo}]}]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writePipeline(t, t.TempDir(), test.pipeline)

			err := schema.Validate(yamlAsJSONValue(t, path))
			if err == nil {
				t.Errorf("schema accepted an invalid pipeline:\n%s", test.pipeline)
			}
		})
	}
}

// The schema's step properties must match config.Step's yaml tags. Adding a
// field to the struct without adding it here would leave editors flagging
// valid pipelines as errors — a silent, annoying kind of wrong.
func TestSchemaStepKeysMatchStruct(t *testing.T) {
	data, err := os.ReadFile("steps.schema.json")
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Defs struct {
			Step struct {
				Properties map[string]any `json:"properties"`
			} `json:"step"`
		} `json:"$defs"`
	}

	err = json.Unmarshal(data, &doc)
	if err != nil {
		t.Fatal(err)
	}

	inSchema := doc.Defs.Step.Properties
	inStruct := yamlTagNames(reflect.TypeOf(config.Step{}))

	for name := range inStruct {
		if _, ok := inSchema[name]; !ok {
			t.Errorf("config.Step has yaml key %q with no property in steps.schema.json", name)
		}
	}

	for name := range inSchema {
		if _, ok := inStruct[name]; !ok {
			t.Errorf("steps.schema.json declares step property %q that config.Step does not have", name)
		}
	}
}

// yamlTagNames collects the yaml key of every field on a struct, following
// `yaml:",inline"` fields (Hooks) into the parent's key space, and skipping
// `yaml:"-"` (computed, never written).
func yamlTagNames(structType reflect.Type) map[string]struct{} {
	out := map[string]struct{}{}

	for i := range structType.NumField() {
		tag := structType.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}

		name, options, _ := strings.Cut(tag, ",")

		if strings.Contains(options, "inline") {
			field := structType.Field(i).Type
			for inlined := range yamlTagNames(field) {
				out[inlined] = struct{}{}
			}

			continue
		}

		if name != "" {
			out[name] = struct{}{}
		}
	}

	return out
}
