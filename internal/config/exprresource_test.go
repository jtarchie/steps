package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeExprPipeline writes a pipeline (and any extra files) into a temp dir
// and loads it, returning the config and error for the caller to judge.
func loadExprPipeline(t *testing.T, pipeline string, files map[string]string) (*Config, error) {
	t.Helper()

	dir := t.TempDir()

	for name, contents := range files {
		full := filepath.Join(dir, name)

		err := os.MkdirAll(filepath.Dir(full), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(full, []byte(contents), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return LoadConfig(path)
}

func TestExprBackendLoads(t *testing.T) {
	t.Parallel()

	cfg, err := loadExprPipeline(t, `
resource_types:
- name: api
  env: [API_TOKEN]
  config:
    expr:
      check: '[{ref: "v1"}]'
      in: '{"version.json": toJSON(version)}'
      out: 'nil'

resources:
- name: thing
  type: api
  source: {}

jobs:
- name: build
  plan:
  - get: thing
  - put: thing
`, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := cfg.ResourceTypes[0].Config.Backend(); got != BackendExpr {
		t.Errorf("Backend() = %q, want %q", got, BackendExpr)
	}
}

// TestExprFileIncludes covers the reason the _file siblings exist: a
// twenty-line program belongs in a file a diff and a review comment can
// address. Resolution happens before validate and before hashing, so the
// loaded config is indistinguishable from one written inline.
func TestExprFileIncludes(t *testing.T) {
	t.Parallel()

	cfg, err := loadExprPipeline(t, `
resource_types:
- name: api
  config:
    expr:
      check_file: types/check.expr
      in_file: types/in.expr

resources:
- name: thing
  type: api
  source: {}

jobs:
- name: build
  plan:
  - get: thing
`, map[string]string{
		"types/check.expr": "[{ref: \"v1\"}]\n",
		"types/in.expr":    "{\"version.json\": toJSON(version)}\n",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	expression := cfg.ResourceTypes[0].Config.Expr
	if !strings.Contains(expression.Check, `ref: "v1"`) {
		t.Errorf("check = %q, want the file's contents inlined", expression.Check)
	}

	if !strings.Contains(expression.In, "version.json") {
		t.Errorf("in = %q, want the file's contents inlined", expression.In)
	}
}

func TestExprFileIncludeMissingFile(t *testing.T) {
	t.Parallel()

	_, err := loadExprPipeline(t, `
resource_types:
- name: api
  config:
    expr:
      check_file: types/nope.expr

resources:
- name: thing
  type: api
  source: {}

jobs:
- name: build
  plan:
  - get: thing
`, nil)
	if err == nil || !strings.Contains(err.Error(), "check_file") {
		t.Fatalf("err = %v, want an error naming check_file", err)
	}
}

// TestExprSyntaxErrorIsNotALoadError states the seam out loud. internal/config
// depends on nothing internal and on no third-party code but the YAML parser,
// which is what lets every other package share these types without inheriting
// an expression engine. So an unparsable expression LOADS, and is caught by
// `steps validate` and preflight instead (resource.CompileExprPrograms).
func TestExprSyntaxErrorIsNotALoadError(t *testing.T) {
	t.Parallel()

	_, err := loadExprPipeline(t, `
resource_types:
- name: api
  config:
    expr:
      check: 'source.items | map('

resources:
- name: thing
  type: api
  source: {}

jobs:
- name: build
  plan:
  - get: thing
`, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v, want an unparsable expression to LOAD (it is caught at validate)", err)
	}
}

func TestExprBackendMutualExclusion(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"shell and expr": `
resource_types:
- name: api
  config:
    check: printf '[]'
    expr:
      check: '[]'
`,
		"mcp and expr": `
mcp_servers:
- name: srv
  endpoint: https://example.com/mcp
resource_types:
- name: api
  config:
    mcp:
      server: srv
      check: {tool: list}
    expr:
      check: '[]'
`,
	}

	for name, block := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := loadExprPipeline(t, block+`
resources:
- name: thing
  type: api
  source: {}

jobs:
- name: build
  plan:
  - get: thing
`, nil)
			if err == nil || !strings.Contains(err.Error(), "exactly one backend") {
				t.Fatalf("err = %v, want the one-backend rule", err)
			}
		})
	}
}

func TestExprRejectsContainerSettings(t *testing.T) {
	t.Parallel()

	for _, setting := range []string{
		"image: alpine:3",
		"privileged: true",
		"user: nobody",
	} {
		_, err := loadExprPipeline(t, `
resource_types:
- name: api
  `+setting+`
  config:
    expr:
      check: '[]'

resources:
- name: thing
  type: api
  source: {}

jobs:
- name: build
  plan:
  - get: thing
`, nil)
		if err == nil || !strings.Contains(err.Error(), "evaluates in-process") {
			t.Errorf("%s: err = %v, want the in-process rule", setting, err)
		}
	}
}

// TestBackendTable pins the mapping every tagged dispatch switch depends on.
// Adding a backend without updating this is the change `exhaustive` then
// reports at every site.
func TestBackendTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config ResourceTypeConfig
		want   ResourceBackend
	}{
		{"shell", ResourceTypeConfig{Check: "printf '[]'"}, BackendShell},
		{"empty is shell", ResourceTypeConfig{}, BackendShell},
		{"mcp", ResourceTypeConfig{MCP: &MCPResourceConfig{Server: "s"}}, BackendMCP},
		{"expr", ResourceTypeConfig{Expr: &ExprResourceConfig{Check: "[]"}}, BackendExpr},
	}

	for _, test := range tests {
		if got := test.config.Backend(); got != test.want {
			t.Errorf("%s: Backend() = %q, want %q", test.name, got, test.want)
		}
	}
}
