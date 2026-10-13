package e2e

import (
	"fmt"
	"os"
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

// TestUnresolvedVarNestedInThePlanIsALoadError: the load check used to read
// only the top-level plan, so a typo inside a hook or a block loaded fine and
// reached the shell as the literal text.
func TestUnresolvedVarNestedInThePlanIsALoadError(t *testing.T) {
	for name, plan := range map[string]string{
		"step hook": `
  - task: build
    run: "true"
    on_failure:
      task: notify
      run: echo ((typo_nested))`,
		"job hook": `
  - task: build
    run: "true"
  ensure:
    task: notify
    run: echo ((typo_nested))`,
		"do child": `
  - do:
    - task: first
      run: "true"
    - task: second
      run: echo ((typo_nested))`,
		"try body": `
  - try:
      task: first
      run: echo ((typo_nested))`,
		"in_parallel branch": `
  - in_parallel:
      steps:
      - task: first
        run: echo ((typo_nested))`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writePipeline(t, t.TempDir(), "jobs:\n- name: build\n  plan:"+plan+"\n")

			err := cli.Run([]string{"validate", path})
			if err == nil {
				t.Fatal("a pipeline with an unsupplied nested var validated")
			}

			if !strings.Contains(err.Error(), "((typo_nested))") {
				t.Errorf("error does not name the missing var: %v", err)
			}
		})
	}
}

// TestNestedLoadVarFeedsLaterSteps: a load_var: inside a do: block or a
// concurrent branch has run by the time the plan moves past the block, so
// the steps after it may use the value.
func TestNestedLoadVarFeedsLaterSteps(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tag.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: pick
    outputs: [meta]
    run: printf 'v1.2.3\n' > meta/version.txt && printf 'blue\n' > meta/color.txt
  - do:
    - load_var: tag
      file: meta/version.txt
      inputs: [meta]
    - task: inside
      run: echo "inside ((tag))" >> %[1]s
  - in_parallel:
      steps:
      - load_var: color
        file: meta/color.txt
        inputs: [meta]
      - task: sibling
        run: "true"
  - task: after
    run: echo "after ((tag)) ((color))" >> %[1]s
`, log))

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := readFileString(t, log)
	if !strings.Contains(got, "inside v1.2.3") || !strings.Contains(got, "after v1.2.3 blue") {
		t.Errorf("log = %q, want both nested captures substituted", got)
	}
}

// TestParallelSiblingCannotUseABranchesLoadVar: in_parallel branches run at
// once, so whether one sees another's capture is a race. Refused at load
// rather than left to timing.
func TestParallelSiblingCannotUseABranchesLoadVar(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: pick
    outputs: [meta]
    run: printf 'v1\n' > meta/version.txt
  - in_parallel:
      steps:
      - load_var: tag
        file: meta/version.txt
        inputs: [meta]
      - task: racer
        run: echo ((tag))
`)

	err := cli.Run([]string{"validate", path})
	if err == nil {
		t.Fatal("a branch using a sibling branch's load_var validated")
	}

	if !strings.Contains(err.Error(), "((tag))") || !strings.Contains(err.Error(), "in_parallel branch 1") {
		t.Errorf("error does not name the var and the branch: %v", err)
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

// TestLoadVarFileOutsideTheWorkspaceIsALoadError covers a literal file: that
// climbs out of the step's directory: the read happens on this machine, so it
// would otherwise capture whatever sits beside the workspace.
func TestLoadVarFileOutsideTheWorkspaceIsALoadError(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: pick-tag
    inputs: []
    outputs: [meta]
    run: "true"
  - load_var: tag
    file: meta/../../something
    inputs: [meta]
`)

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("a load_var whose file: leaves the workspace ran")
	}

	if !strings.Contains(err.Error(), `load_var "tag" file "meta/../../something" escapes the workspace`) {
		t.Errorf("error does not name the step and the path: %v", err)
	}
}

// TestLoadVarFileSteeredOutByAVarFailsTheStep covers the path load cannot see:
// a file: rendered from a value an earlier step captured. The target exists
// and the read would succeed, so only the confinement can fail it.
func TestLoadVarFileSteeredOutByAVarFailsTheStep(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	writePipelineFile(t, secret, "TOPSECRET\n")

	log := filepath.Join(dir, "stolen.log")
	climb := strings.Repeat("../", 64) + strings.TrimPrefix(secret, "/")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: aim
    inputs: []
    outputs: [meta]
    run: printf '%%s' '%s' > meta/up
  - load_var: up
    file: meta/up
    inputs: [meta]
  - load_var: stolen
    file: meta/((up))
    inputs: [meta]
  - task: leak
    inputs: []
    run: echo "((stolen))" >> %s
`, climb, log))

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("a load_var steered out of its workspace by a var ran")
	}

	if !strings.Contains(err.Error(), `load_var "stolen"`) || !strings.Contains(err.Error(), "escapes the workspace") {
		t.Errorf("error does not name the step and say the path escapes: %v", err)
	}

	if got, _ := os.ReadFile(log); strings.Contains(string(got), "TOPSECRET") { //nolint:gosec // a t.TempDir() file the test's own pipeline wrote
		t.Errorf("the file outside the workspace was captured: %q", got)
	}
}

// TestLoadVarRefusesAnOversizeFile covers the unbounded read: a var is a value
// substituted into later steps, not a data transfer.
func TestLoadVarRefusesAnOversizeFile(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: pick-tag
    inputs: []
    outputs: [meta]
    run: head -c 1048577 /dev/zero | tr '\0' a > meta/big
  - load_var: tag
    file: meta/big
    inputs: [meta]
`)

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("a load_var read a file above the limit")
	}

	if !strings.Contains(err.Error(), `load_var "tag"`) || !strings.Contains(err.Error(), "meta/big") || !strings.Contains(err.Error(), "above the limit") {
		t.Errorf("error does not name the step, the path and the limit: %v", err)
	}
}

// TestLoadVarRefusesASymlinkOutOfTheWorkspace covers what a lexical check
// cannot see: a file: inside the workspace that is a link to one outside it.
func TestLoadVarRefusesASymlinkOutOfTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	writePipelineFile(t, secret, "TOPSECRET\n")

	log := filepath.Join(dir, "stolen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: aim
    inputs: []
    outputs: [meta]
    run: ln -s %s meta/link
  - load_var: stolen
    file: meta/link
    inputs: [meta]
  - task: leak
    inputs: []
    run: echo "((stolen))" >> %s
`, secret, log))

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("a load_var followed a symlink out of its workspace")
	}

	if !strings.Contains(err.Error(), `load_var "stolen"`) || !strings.Contains(err.Error(), "meta/link") {
		t.Errorf("error does not name the step and the path: %v", err)
	}

	if got, _ := os.ReadFile(log); strings.Contains(string(got), "TOPSECRET") { //nolint:gosec // a t.TempDir() file the test's own pipeline wrote
		t.Errorf("the file outside the workspace was captured: %q", got)
	}
}
