package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// steps validate reports everything wrong with a pipeline in one pass, and —
// the point of the command — touches nothing while doing it: no state store,
// no workspace, no containers, no step executed.
func TestValidateReportsAllErrorsAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: j
  plan:
  - task: t
    run: touch `+filepath.Join(dir, "ran.txt")+`
    trigger: true
  - agent: ghost
    prompt: x
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("expected validate to fail on a broken pipeline")
	}

	for _, want := range []string{
		"trigger is only valid on get steps",
		`no agent named "ghost"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q\n  is missing %q", err.Error(), want)
		}
	}

	// Nothing ran and nothing was persisted: validating is a read.
	_, statErr := os.Stat(filepath.Join(dir, "ran.txt"))
	if statErr == nil {
		t.Error("validate executed a step")
	}

	_, statErr = os.Stat(filepath.Join(dir, ".steps"))
	if statErr == nil {
		t.Error("validate created a state store")
	}
}

func TestValidateAcceptsAValidPipeline(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: j
  plan:
  - task: greet
    run: echo hello
`)

	err := run([]string{"validate", path})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// Artifact flow is checked for every job, not just the one a run would have
// selected — the whole file is the unit being validated.
func TestValidateChecksArtifactFlowInEveryJob(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
workspace:
  strategy: copy
jobs:
- name: fine
  plan:
  - task: a
    run: "true"
    outputs: [built]
  - task: b
    run: "true"
    inputs: [built]
- name: broken
  plan:
  - task: c
    run: "true"
    inputs: [never-produced]
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("expected validate to reject an input nothing produces")
	}

	if !strings.Contains(err.Error(), "never-produced") {
		t.Errorf("error = %q, want it to name the undeclared artifact", err.Error())
	}
}

// Every shipped example validates, so `steps validate examples/<x>.yml` is a
// working first command for a reader who just copied one.
//
// The dummy keys are load-bearing: validate now checks that the credentials a
// pipeline names are actually present, and the agent examples name one.
// Setting them here (rather than passing --syntax-only) keeps the full path
// under test — the examples would otherwise only ever be checked with half of
// validate switched off.
//
// One per PROVIDER any example reaches for. An example that names a provider
// with no key here fails this test rather than silently skipping the check,
// which is how pointing pr-review.yml at opencode's Go models was caught.
func TestValidateExamples(t *testing.T) {
	for _, key := range []string{"OPENROUTER_API_KEY", "OPENCODE_API_KEY"} {
		t.Setenv(key, "test-key-not-used-for-any-call")
	}

	matches, err := filepath.Glob("examples/*.yml")
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) == 0 {
		t.Fatal("no examples found")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			err := run([]string{"validate", path})
			if err != nil {
				t.Errorf("validate %s: %v", path, err)
			}
		})
	}
}

// A built-in agent referenced with only a source: reaches the model carrying
// the profile's persona and tool grant. This is the whole point of the
// @builtin merge: the pipeline supplies the one thing it must (which model),
// and gets the prompt and tools it referenced the profile for.
func TestEndToEndBuiltinAgentSuppliesOnlyTheModel(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t, says("Looks fine."))

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: "@builtin/reviewer"
  source:
    model: openai/test-model
    endpoint: `+fake.URL+`
    api_key_env: STEPS_TEST_AGENT_API_KEY
jobs:
- name: review
  plan:
  - agent: "@builtin/reviewer"
    prompt: review the notes
`)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	req := fake.request(1)

	if req.systemMessage() == "" {
		t.Error("no system message on the wire; the built-in persona was lost")
	}

	// reviewer is the read-only profile: it grants read tools and
	// deliberately no shell or write access.
	got := req.toolNames()
	for _, want := range []string{"read_file", "list_dir", "search_files"} {
		if !slices.Contains(got, want) {
			t.Errorf("tools on the wire = %v, want it to include %q", got, want)
		}
	}

	for _, unwanted := range []string{"run_shell", "write_file", "edit_file"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("tools on the wire = %v, want the read-only profile to withhold %q", got, unwanted)
		}
	}
}

// steps runs reads back what a run recorded. Everything it prints was already
// being written and had no reader: the only route to "why did my last run
// fail" was opening .steps/state.db by hand and knowing the schema.
func TestRunsReportsHistory(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: boom
    run: exit 3
`)

	// The run fails; that outcome is what we want to read back.
	err := run([]string{path})
	if err == nil {
		t.Fatal("expected the pipeline to fail")
	}

	out := captureStdout(t, func() {
		runsErr := run([]string{"runs", path})
		if runsErr != nil {
			t.Fatalf("runs: %v", runsErr)
		}
	})

	for _, want := range []string{"build", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("runs output %q\n  is missing %q", out, want)
		}
	}

	steps := captureStdout(t, func() {
		runsErr := run([]string{"runs", path, "--steps"})
		if runsErr != nil {
			t.Fatalf("runs --steps: %v", runsErr)
		}
	})

	if !strings.Contains(steps, "boom") {
		t.Errorf("runs --steps output %q\n  is missing the failing step name", steps)
	}
}

// Asking about history on a pipeline that has never run says so, and — since
// this is a read — does not create a state store as a side effect.
func TestRunsOnAFreshPipelineWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan: [{ task: t, run: "true" }]
`)

	out := captureStdout(t, func() {
		err := run([]string{"runs", path})
		if err != nil {
			t.Fatalf("runs: %v", err)
		}
	})

	if !strings.Contains(out, "no runs recorded yet") {
		t.Errorf("output = %q, want it to say there is no history yet", out)
	}

	_, err := os.Stat(filepath.Join(dir, ".steps"))
	if err == nil {
		t.Error("runs created a state store; reading history must not write")
	}
}

// TestValidateRejectsAPipelineThatCannotRunHere covers the gap that made `ok`
// misleading: `steps validate` reported ok for a pipeline with an unset API
// key and an MCP server whose binary is not installed. Both are yes/no facts
// answerable in microseconds, and both used to surface at run time — for an
// agent step, after it had already started billing.
func TestValidateRejectsAPipelineThatCannotRunHere(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
mcp_servers:
- name: nope
  command: definitely-not-a-real-binary-xyz

agents:
- name: writer
  source:
    model: openai/gpt-4o
    api_key_env: STEPS_TEST_DEFINITELY_UNSET_KEY

jobs:
- name: publish
  plan:
  - agent: writer
    inputs: []
    prompt: Write something.
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("validate reported ok for a pipeline that cannot run")
	}

	// Every problem, not just the first: finding them one run at a time is
	// the failure mode this exists to end.
	for _, want := range []string{
		"STEPS_TEST_DEFINITELY_UNSET_KEY is not set",
		`mcp "nope"`,
		"not found on PATH",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q\n  is missing %q", err.Error(), want)
		}
	}
}

// TestValidateAcceptsAPipelineWhoseCredentialsArePresent is the other half:
// the check must pass once the environment actually has what the pipeline
// needs, or `steps validate` becomes unusable in exactly the CI setting it is
// meant for.
func TestValidateAcceptsAPipelineWhoseCredentialsArePresent(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: writer
  source:
    model: openai/gpt-4o
    api_key_env: STEPS_TEST_PRESENT_KEY

jobs:
- name: publish
  plan:
  - agent: writer
    inputs: []
    prompt: Write something.
`)

	t.Setenv("STEPS_TEST_PRESENT_KEY", "set-to-something")

	err := run([]string{"validate", path})
	if err != nil {
		t.Fatalf("validate rejected a runnable pipeline: %v", err)
	}
}

// TestValidateRejectsAnUnknownProviderPrefix pins the static half at LOAD
// time, not just in validate: a typo in a model name (`opencoder/` for
// `opencode/`) should cost a load, not a run billed per token.
func TestValidateRejectsAnUnknownProviderPrefix(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: writer
  source:
    model: notaprovider/some-model

jobs:
- name: publish
  plan:
  - agent: writer
    inputs: []
    prompt: Write something.
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("validate reported ok for a model with no known provider prefix")
	}

	if !strings.Contains(err.Error(), "no known provider prefix") {
		t.Errorf("error does not name the unknown prefix: %v", err)
	}
}

// TestValidateSyntaxOnlySkipsMachineChecks covers the lint-in-CI case: a
// pre-commit hook or a build that checks a pipeline it has no intention of
// running should not need that pipeline's production credentials on hand.
func TestValidateSyntaxOnlySkipsMachineChecks(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
mcp_servers:
- name: nope
  command: definitely-not-a-real-binary-xyz

agents:
- name: writer
  source:
    model: openai/gpt-4o
    api_key_env: STEPS_TEST_DEFINITELY_UNSET_KEY

jobs:
- name: publish
  plan:
  - agent: writer
    inputs: []
    prompt: Write something.
`)

	err := run([]string{"validate", path, "--syntax-only"})
	if err != nil {
		t.Fatalf("--syntax-only still checked this machine: %v", err)
	}

	// The same file without the flag must still be rejected, or the flag is
	// not skipping anything.
	err = run([]string{"validate", path})
	if err == nil {
		t.Fatal("validate without --syntax-only reported ok for a pipeline that cannot run")
	}
}

// TestEveryDocumentedCommandIsReachable checks every command docs/README.md
// advertises actually parses.
//
// It exists because two commands shipped unreachable: the CLI struct field
// that registers them was never added, so `steps jobs` and `steps approve`
// parsed as the default run command with a nonsense pipeline argument.
// Everything compiled and every unit test passed — the code was there, the
// grammar was not.
//
// Driving it from the docs rather than from the struct is deliberate: a
// struct-driven test can only check what is registered, which is precisely the
// half that was fine. The docs are where a command is promised.
func TestEveryDocumentedCommandIsReachable(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("docs", "README.md"))
	if err != nil {
		t.Fatal(err)
	}

	commands := documentedCommands(string(body))
	if len(commands) < 5 {
		t.Fatalf("found %d documented commands, expected the whole list — has the docs format changed?", len(commands))
	}

	for _, name := range commands {
		t.Run(name, func(t *testing.T) {
			// The command name alone, with none of its required arguments.
			// A recognized command fails in the PARSER, complaining about the
			// argument it is missing. An unrecognized one is swallowed by the
			// default run command, which treats the word as a pipeline path
			// and fails trying to open it — so that error is the signature of
			// a command that does not exist. (--help would be the obvious
			// probe, but kong exits the process for it.)
			runErr := run([]string{name})
			if runErr != nil && strings.Contains(runErr.Error(), "could not load pipeline") {
				t.Errorf("docs/README.md advertises `steps %s`, but it parsed as the default command with %q as its pipeline: %v",
					name, name, runErr)
			}
		})
	}
}

// documentedCommands pulls the command names out of docs/README.md's command
// list — the lines of the form "steps <name> ...".
func documentedCommands(body string) []string {
	var (
		names []string
		seen  = map[string]bool{}
	)

	for line := range strings.Lines(body) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "steps" {
			continue
		}

		// "steps mcp tools|login" documents one command with two verbs; the
		// first word is the command either way.
		name := strings.TrimSuffix(fields[1], ",")
		if strings.HasPrefix(name, "<") || seen[name] {
			continue
		}

		seen[name] = true
		names = append(names, name)
	}

	return names
}
