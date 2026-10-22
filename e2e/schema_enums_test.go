package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// schemaEnumValues collects the enum of every property named key in the schema document.
func schemaEnumValues(node any, key string) []string {
	var found []string

	switch typed := node.(type) {
	case map[string]any:
		for name, child := range typed {
			if props, ok := child.(map[string]any); ok && name == key {
				enum, _ := props["enum"].([]any)
				for _, v := range enum {
					found = append(found, fmt.Sprint(v))
				}
			}

			found = append(found, schemaEnumValues(child, key)...)
		}
	case []any:
		for _, child := range typed {
			found = append(found, schemaEnumValues(child, key)...)
		}
	}

	return found
}

func enumPipelines(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(repoFile("steps.schema.json"))
	if err != nil {
		t.Fatal(err)
	}

	var doc any

	err = json.Unmarshal(raw, &doc)
	if err != nil {
		t.Fatal(err)
	}

	const jobs = "jobs: [{name: j, plan: [{task: t, run: 'true'}]}]\n"

	pipelines := make([]string, 0, 16)

	// The loader lowercases reasoning_effort, so any casing must pass the schema too.
	for _, effort := range []string{"low", "medium", "high", "Low", "MEDIUM", "hIgH"} {
		pipelines = append(pipelines, "agents: [{name: a, source: {model: openai/gpt-4o}, reasoning_effort: "+effort+"}]\n"+jobs)
	}

	for _, value := range schemaEnumValues(doc, "compression") {
		pipelines = append(pipelines, "workspace: {strategy: btrfs, root: /tmp/r, options: {compression: "+value+"}}\n"+jobs)
	}

	for _, value := range schemaEnumValues(doc, "strategy") {
		pipelines = append(pipelines, "workspace: {strategy: "+value+", root: /tmp/r}\n"+jobs)
	}

	return pipelines
}

// Every value the schema admits for a field the loader also validates must load, so the hand-written enums cannot drift from the Go validators (reasoning_effort casing did).
func TestSchemaEnumsAgreeWithLoaderValidators(t *testing.T) {
	schema := loadSchema(t)

	pipelines := enumPipelines(t)
	if len(pipelines) < 12 {
		t.Fatalf("only %d pipelines built; the enum walk found too little", len(pipelines))
	}

	for _, pipeline := range pipelines {
		t.Run(strings.SplitN(pipeline, "\n", 2)[0], func(t *testing.T) {
			path := writePipeline(t, t.TempDir(), pipeline)

			err := schema.Validate(yamlAsJSONValue(t, path))
			if err != nil {
				t.Errorf("schema rejects a value the loader accepts: %v", err)
			}

			_, err = config.Load(path, "p", nil)
			if err != nil {
				t.Errorf("loader rejects a value the schema lists: %v", err)
			}
		})
	}
}
