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
func TestValidateExamples(t *testing.T) {
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
