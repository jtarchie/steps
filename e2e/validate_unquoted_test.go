package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// unquotedPipeline renders a check: and a custom tool's run: from the two
// template expressions it is handed.
const unquotedPipeline = `
resource_types:
- name: refs
  config:
    check: |
      echo checking after %s
      printf '[{"ref": "v1"}]'
    in: |
      echo {{ .version.ref | shellquote }} > ref.txt

resources:
- name: repo
  type: refs
  source: {}

agents:
- name: poster
  source: { model: openai/gpt-4o }
  tools:
  - name: post
    description: post a body
    run: echo %s

jobs:
- name: build
  plan:
  - get: repo
`

// A version field comes from check output — a PR title, a Slack message — so
// one rendered bare into sh -c is shell syntax somebody else wrote. validate
// says so, and still passes: quoting is a convention, not a load rule.
func TestValidateWarnsOnUnquotedShellTemplateValues(t *testing.T) {
	t.Parallel()

	t.Run("unquoted", func(t *testing.T) {
		stderr := captureStderr(t)
		path := writePipeline(t, t.TempDir(), fmt.Sprintf(unquotedPipeline, "{{ .version.ref }}", "{{ .args.body }}"))

		err := stepsRun(t, []string{"validate", path, "--syntax-only"})
		if err != nil {
			t.Fatalf("a warning failed validate: %v", err)
		}

		got := stderr()
		for _, want := range []string{
			`warning: resource type "refs" check: {{.version.ref}} reaches the shell unquoted`,
			`warning: agent "poster" tool "post" run: {{.args.body}} reaches the shell unquoted`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("stderr is missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("quoted", func(t *testing.T) {
		stderr := captureStderr(t)
		path := writePipeline(t, t.TempDir(), fmt.Sprintf(unquotedPipeline, "{{ .version.ref | shellquote }}", "{{ .args.body | shellquote }}"))

		err := stepsRun(t, []string{"validate", path, "--syntax-only"})
		if err != nil {
			t.Fatalf("validate: %v", err)
		}

		if got := stderr(); strings.Contains(got, "warning:") {
			t.Errorf("a quoted pipeline warned:\n%s", got)
		}
	})
}

// The plain validate — no flags — is the one an author runs, and a warning
// must not cost it its exit status.
func TestValidateUnquotedWarningExitsZero(t *testing.T) {
	t.Parallel()

	stderr := captureStderr(t)
	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: refs
  config:
    check: |
      git ls-remote origin {{ .version.ref }}
      printf '[{"ref": "v1"}]'

resources:
- name: repo
  type: refs
  source: {}

jobs:
- name: build
  plan:
  - get: repo
`)

	err := stepsRun(t, []string{"validate", path})
	if err != nil {
		t.Fatalf("a warning failed validate: %v", err)
	}

	if got := stderr(); !strings.Contains(got, `warning: resource type "refs" check: {{.version.ref}} reaches the shell unquoted`) {
		t.Errorf("no warning:\n%s", got)
	}
}

// An export's caller is not the pipeline's author, so its unquoted value is refused, not warned about — and refused however the template spells its way to the value.
func TestValidateRefusesAnExportThatReachesACallersValueByAnotherSpelling(t *testing.T) {
	t.Parallel()

	for _, run := range []string{
		`echo {{ index . "args" "note" }}`,
		`{{ define "say" }}echo {{ . }}{{ end }}{{ template "say" .args.note }}`,
	} {
		path := writePipeline(t, t.TempDir(), `
mcp_exports:
- name: shell
  access: "true"
  tools:
  - name: say
    description: say it
    run: '`+strings.ReplaceAll(run, "'", "''")+`'

jobs:
- name: build
  plan:
  - task: hi
    run: echo hi
`)

		err := stepsRun(t, []string{"validate", path, "--syntax-only"})
		if err == nil || !strings.Contains(err.Error(), "unquoted") {
			t.Errorf("validate of %s = %v, want it refused as unquoted", run, err)
		}
	}
}
