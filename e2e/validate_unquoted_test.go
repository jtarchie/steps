package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
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
	t.Run("unquoted", func(t *testing.T) {
		stderr := captureStderr(t)
		path := writePipeline(t, t.TempDir(), fmt.Sprintf(unquotedPipeline, "{{ .version.ref }}", "{{ .args.body }}"))

		err := cli.Run([]string{"validate", path, "--syntax-only"})
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

		err := cli.Run([]string{"validate", path, "--syntax-only"})
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

	err := cli.Run([]string{"validate", path})
	if err != nil {
		t.Fatalf("a warning failed validate: %v", err)
	}

	if got := stderr(); !strings.Contains(got, `warning: resource type "refs" check: {{.version.ref}} reaches the shell unquoted`) {
		t.Errorf("no warning:\n%s", got)
	}
}
