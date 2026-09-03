package e2e

import (
	"encoding/json"
	"os"
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

	data, err := os.ReadFile(repoFile("steps.schema.json"))
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

// The tested example corpus lives in docs/*.md; every extracted block is
// schema-validated by TestDocsExamples (docs_test.go), which is what keeps
// the hand-written schema honest against the loader.

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

// schemaDefsByType maps a $defs entry in steps.schema.json to the config type
// it describes. Every entry is held key-for-key against the struct below.
//
// The three unlisted object defs are the ones with no struct to compare
// against: agentSource/fileRef/toolSpec and friends decode through
// hand-written UnmarshalYAML, so reflection reports no yaml tags for them.
func schemaDefsByType() map[string]reflect.Type {
	return map[string]reflect.Type{
		"step":         reflect.TypeOf(config.Step{}),
		"job":          reflect.TypeOf(config.Job{}),
		"task":         reflect.TypeOf(config.Task{}),
		"agent":        reflect.TypeOf(config.Agent{}),
		"resource":     reflect.TypeOf(config.Resource{}),
		"resourceType": reflect.TypeOf(config.ResourceType{}),
		"mcpServer":    reflect.TypeOf(config.MCPServer{}),
		// Listed where mcpResourceConfig is not, because this one decodes
		// with plain yaml tags and so CAN be compared by reflection — the
		// unlisted defs are the ones whose members go through hand-written
		// UnmarshalYAML.
		"exprResourceConfig": reflect.TypeOf(config.ExprResourceConfig{}),
		"assert":             reflect.TypeOf(config.Assert{}),
		"defaults":           reflect.TypeOf(config.Defaults{}),
		"workspace":          reflect.TypeOf(config.WorkspaceConfig{}),
	}
}

// The schema's properties must match the config structs' yaml tags, in both
// directions and for every type the pipeline format exposes. A field added to
// a struct without a property here leaves editors flagging valid pipelines as
// errors — a silent, annoying kind of wrong — and a property here with no
// field offers completion for a key the loader will reject.
func TestSchemaKeysMatchStructs(t *testing.T) {
	data, err := os.ReadFile(repoFile("steps.schema.json"))
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Properties map[string]any `json:"properties"`
		Defs       map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"$defs"`
	}

	err = json.Unmarshal(data, &doc)
	if err != nil {
		t.Fatal(err)
	}

	for def, typ := range schemaDefsByType() {
		entry, ok := doc.Defs[def]
		if !ok {
			t.Errorf("steps.schema.json has no $defs/%s", def)

			continue
		}

		compareKeys(t, "$defs/"+def, typ, entry.Properties)
	}

	// The document root is config.Config itself, spelled as top-level
	// properties rather than a $defs entry.
	compareKeys(t, "the schema root", reflect.TypeOf(config.Config{}), doc.Properties)
}

// compareKeys holds one struct's yaml tags and one schema object's properties
// in agreement, reporting both directions of drift.
func compareKeys(t *testing.T, where string, structType reflect.Type, inSchema map[string]any) {
	t.Helper()

	inStruct := yamlTagNames(structType)

	for name := range inStruct {
		if _, ok := inSchema[name]; !ok {
			t.Errorf("%s: config.%s has yaml key %q with no property in steps.schema.json", where, structType.Name(), name)
		}
	}

	for name := range inSchema {
		if _, ok := inStruct[name]; !ok {
			t.Errorf("%s: steps.schema.json declares property %q that config.%s does not have", where, name, structType.Name())
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
