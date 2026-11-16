package e2e

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func enumPipelines(t *testing.T) []string {
	t.Helper()

	const jobs = "jobs: [{name: j, plan: [{task: t, run: 'true'}]}]\n"

	pipelines := make([]string, 0, 16)

	// The loader lowercases reasoning_effort, so any casing must pass the schema too.
	for _, effort := range []string{"low", "medium", "high", "Low", "MEDIUM", "hIgH"} {
		pipelines = append(pipelines, "agents: [{name: a, source: {model: openai/gpt-4o}, reasoning_effort: "+effort+"}]\n"+jobs)
	}

	return pipelines
}

// Every value the schema admits for a field the loader also validates must load, so the hand-written enums cannot drift from the Go validators (reasoning_effort casing did).
func TestSchemaEnumsAgreeWithLoaderValidators(t *testing.T) {
	t.Parallel()

	schema := loadSchema(t)

	pipelines := enumPipelines(t)
	if len(pipelines) < 6 {
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
