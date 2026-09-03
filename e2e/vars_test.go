package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestVarsInterpolateFromTheCommandLine is the shape the story describes: one
// pipeline file serving staging and production instead of being copy-pasted
// per environment and drifting.
func TestVarsInterpolateFromTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "target.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: announce
    inputs: []
    run: echo ((target)) >> %s
`, log))

	err := cli.Run([]string{"run", path, "--job", "build", "--var", "target=staging"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := readFileString(t, log); !strings.Contains(got, "staging") {
		t.Errorf("the var was not substituted; log says %q", got)
	}
}

// TestVarsFileAndFlagCompose verifies the flag overrides the file — the only
// ordering that makes a one-off override possible at all.
func TestVarsFileAndFlagCompose(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "target.log")

	varsFile := filepath.Join(dir, "vars.yml")
	writePipelineFile(t, varsFile, "target: from-file\nother: also-from-file\n")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: announce
    inputs: []
    run: echo ((target)) ((other)) >> %s
`, log))

	err := cli.Run([]string{"run", path, "--job", "build", "--vars-file", varsFile, "--var", "target=from-flag"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := readFileString(t, log)
	if !strings.Contains(got, "from-flag") || !strings.Contains(got, "also-from-file") {
		t.Errorf("log = %q, want the flag to win and the file to fill the rest", got)
	}
}

// TestUnresolvedVarIsALoadError covers the failure that would otherwise reach
// a shell as the literal text ((repo_uri)) and fail somewhere far from the
// mistake.
func TestUnresolvedVarIsALoadError(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: announce
    inputs: []
    run: echo ((nobody_supplies_this))
`)

	err := cli.Run([]string{"validate", path})
	if err == nil {
		t.Fatal("a pipeline with an unsupplied var validated")
	}

	if !strings.Contains(err.Error(), "nobody_supplies_this") {
		t.Errorf("error does not name the missing var: %v", err)
	}
}

// TestLoadVarCapturesAValueMidRun covers the second mechanism: a value that
// does not exist until the run produces it.
func TestLoadVarCapturesAValueMidRun(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tag.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: pick-tag
    inputs: []
    outputs: [meta]
    run: printf 'v1.2.3\n' > meta/version.txt
  - load_var: tag
    file: meta/version.txt
    inputs: [meta]
  - task: announce
    inputs: []
    run: echo "shipping ((tag))" >> %s
`, log))

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := readFileString(t, log)
	if !strings.Contains(got, "shipping v1.2.3") {
		t.Errorf("log = %q, want the captured value substituted (and its trailing newline trimmed)", got)
	}
}

// TestLoadVarNeedsAFile covers the malformed step.
func TestLoadVarNeedsAFile(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - load_var: tag
`)

	err := cli.Run([]string{"validate", path})
	if err == nil {
		t.Fatal("a load_var with no file validated")
	}

	if !strings.Contains(err.Error(), "needs a file") {
		t.Errorf("error does not explain what is missing: %v", err)
	}
}

// TestVarsWorkOnEveryCommandThatLoadsAPipeline covers the gap that made the
// feature unusable: --var and --vars-file existed only on `steps run`, so
// `steps validate` in CI and `steps web` in production both rejected the
// very pipelines vars were added for — an unresolved ((name)) is a load error,
// and four of the five commands had no way to resolve one.
func TestVarsWorkOnEveryCommandThatLoadsAPipeline(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: announce
    inputs: []
    run: echo ((target))
`)

	// Every command that loads a pipeline without running a job. `run`,
	// `watch` and `test` are excluded only because they would execute
	// something; their flags are wired identically.
	for _, args := range [][]string{
		{"validate", path, "--var", "target=staging"},
		{"plan", path, "--job", "build", "--var", "target=staging"},
		{"preflight", path, "--job", "build", "--var", "target=staging"},
	} {
		t.Run(args[0], func(t *testing.T) {
			err := cli.Run(args)
			if err != nil && strings.Contains(err.Error(), "((target))") {
				t.Errorf("`steps %s` cannot resolve vars: %v", args[0], err)
			}
		})
	}
}
