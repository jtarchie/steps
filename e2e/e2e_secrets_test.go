package e2e

// Secrets cross every seam steps has — config file → resolver, pipeline YAML → reference, run → step environment, and the store that must never see the value — so these start at the daemon and end at the database file's bytes.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/testsshd"
)

const e2eSecretValue = "s3cr3t-value-for-e2e-0f9a"

// secretDaemon serves a daemon whose ci source reads <vault>/<pipeline>/<name>, the layout a 1Password vault per pipeline would have.
func secretDaemon(t *testing.T, dir string, items map[string]string, args ...string) *webProcess {
	t.Helper()

	vault := filepath.Join(dir, "vault")
	writeVault(t, vault, items)

	config := filepath.Join(dir, "steps.yml")
	writeConfigFile(t, config, `
web:
  secret:
    source:
      ci:
        command: cat {{ .path | shellquote }}
        lookup:
        - path: `+vault+`/{{ .pipeline }}/{{ .name }}
`)

	state := filepath.Join(dir, "daemon.db")

	served := startWeb(t, append([]string{"--db", state, "--config", config, "--basic-auth-username", "ops", "--basic-auth-password", "pw"}, args...)...)
	served.state = state

	return served
}

func TestTaskSecretsReachTheStepAndNeverTheDatabase(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	// The expected value sits in a file the pipeline names, never in the pipeline: its text is stored, so writing the value there would put it in the database by itself.
	expected := filepath.Join(dir, "expected")

	err := os.WriteFile(expected, []byte(e2eSecretValue), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: check
    secrets:
      TOKEN: ((ci:gh_token))
    run: test "$TOKEN" = "$(cat `+expected+`)"
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue + "\n"})
	defer served.stopIfRunning(t)

	err = cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	enqueueDirectly(t, served.state, name, "build")
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "build").ID, "succeeded")

	served.stop(t)

	assertNotInDatabase(t, served.state, e2eSecretValue)
}

// assertNotInDatabase reads the files themselves, WAL included: a value that reached any row is in one of them until a checkpoint, and in the main file after.
func assertNotInDatabase(t *testing.T, state, value string) {
	t.Helper()

	for _, suffix := range []string{"", "-wal", "-shm"} {
		body, err := os.ReadFile(state + suffix) //nolint:gosec // the test's own database
		if err != nil {
			continue
		}

		if bytes.Contains(body, []byte(value)) {
			t.Errorf("the secret's value is in %s", filepath.Base(state+suffix))
		}
	}
}

// Sources are configured on the daemon only, so a local run has nowhere to resolve a reference from; failing at load beats a step that runs with an empty variable.
func TestLocalRunRefusesASecretReference(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: check\n    secrets: {TOKEN: ((ci:gh_token))}\n    run: echo hi\n")

	for _, command := range []string{"run", "test"} {
		err := cli.Run([]string{command, "--db", filepath.Join(dir, command+".db"), path})
		if err == nil || !strings.Contains(err.Error(), "steps web") {
			t.Errorf("steps %s = %v, want a refusal pointing at steps web", command, err)
		}
	}

	err := cli.Run([]string{"validate", path})
	if err != nil {
		t.Errorf("steps validate = %v, want the reference accepted", err)
	}
}

func TestPipelineSetRefusesAnUnknownSecretSource(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: check\n    secrets: {TOKEN: ((vault:gh_token))}\n    run: echo hi\n")

	served := secretDaemon(t, dir, nil)
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err == nil || !strings.Contains(err.Error(), `"vault"`) {
		t.Fatalf("pipeline set = %v, want a refusal naming the unknown source", err)
	}
}

// A reference is a secret only where the DSL has a slot for one; anywhere else it would be hashed, stored and rendered into a command.
func TestASecretReferenceOutsideSecretsIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: check\n    run: echo ((ci:gh_token))\n")

	err := cli.Run([]string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), "((ci:gh_token))") || !strings.Contains(err.Error(), "secrets map") {
		t.Fatalf("validate = %v, want a refusal naming the reference and where it belongs", err)
	}
}

// The placed case is the seam the issue named first: the daemon resolves, the worker only ever receives the value, over the session's stdin rather than argv or a request the sshd could log.
func TestPlacedTaskSecretsResolveOnTheDaemon(t *testing.T) {
	server := testsshd.New(t)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	expected := filepath.Join(dir, "expected")

	err := os.WriteFile(expected, []byte(e2eSecretValue), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: remote
    tags: [box]
    secrets:
      TOKEN: ((ci:gh_token))
    run: test "$TOKEN" = "$(cat `+expected+`)"
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue}, "--worker", "box="+server.URL)
	defer served.stopIfRunning(t)

	err = cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	enqueueDirectly(t, served.state, name, "build")
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "build").ID, "succeeded")

	served.stop(t)

	if server.Execs.Load() == 0 {
		t.Error("nothing ran over ssh; the step was not placed")
	}

	if server.EnvRequests.Load() != 0 {
		t.Errorf("the session sent %d env requests; env travels on stdin", server.EnvRequests.Load())
	}

	assertNotInDatabase(t, served.state, e2eSecretValue)
}

// A step that prints its secret, split across two writes so no one chunk holds it whole, has the value scrubbed before anything is recorded or streamed.
func TestPrintedSecretIsScrubbedFromRecordedOutput(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	writePipelineFile(t, path, `
jobs:
- name: release
  plan:
  - task: leak
    secrets:
      TOKEN: ((ci:gh_token))
    run: |
      head=${TOKEN%??????????}
      printf 'token=%s' "$head"
      sleep 0.2
      printf '%s done\n' "${TOKEN#"$head"}"
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue})
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	enqueueDirectly(t, served.state, name, "release")
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "release").ID, "succeeded")

	served.stop(t)

	assertNotInDatabase(t, served.state, e2eSecretValue)
	assertNotInDatabase(t, served.state, e2eSecretValue[:10])

	if !inDatabase(t, served.state, "token=*** done") {
		t.Error("the recorded output does not read token=*** done; the step's output was lost rather than scrubbed")
	}
}

func inDatabase(t *testing.T, state, text string) bool {
	t.Helper()

	for _, suffix := range []string{"", "-wal"} {
		body, err := os.ReadFile(state + suffix) //nolint:gosec // the test's own database
		if err == nil && bytes.Contains(body, []byte(text)) {
			return true
		}
	}

	return false
}

// A fix: agent was never granted the task's secrets: its tools run without them, so nothing it runs can print one into its transcript or hand one to the model.
func TestFixAgentDoesNotInheritTheTasksSecrets(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "fake")

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": `printf 'seen=[%s]' "$TOKEN"`}),
		says("done"),
	)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source:
    endpoint: `+fake.URL+`/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

jobs:
- name: repair
  plan:
  - task: check
    secrets:
      TOKEN: ((ci:gh_token))
    run: exit 1
    fix: fixer
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue})
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	enqueueDirectly(t, served.state, name, "repair")
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "repair").ID, "failed")

	served.stop(t)

	var results []string
	for _, request := range fake.allRequests() {
		results = append(results, request.toolResults()...)
	}

	joined := strings.Join(results, "\n")
	if !strings.Contains(joined, "seen=[]") {
		t.Errorf("the fix agent's run_shell result = %q, want it to have seen no TOKEN", joined)
	}

	if strings.Contains(joined, e2eSecretValue) {
		t.Error("the model was shown the task's secret")
	}

	assertNotInDatabase(t, served.state, e2eSecretValue)
}

// An agent's tools get its secrets, and the model never does: the tool result is scrubbed before it becomes the next message, which is also what the transcript records.
func TestAgentSecretsReachItsToolsButNotTheModel(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "fake")

	dir := t.TempDir()
	expected := filepath.Join(dir, "expected")

	err := os.WriteFile(expected, []byte(e2eSecretValue), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": `test "$TOKEN" = "$(cat ` + expected + `)" && printf 'match:%s' "$TOKEN"`}),
		says("done"),
	)

	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source:
    endpoint: `+fake.URL+`/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
  secrets:
    TOKEN: ((ci:gh_token))

jobs:
- name: review
  plan:
  - agent: reviewer
    messages: [check the token]
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue})
	defer served.stopIfRunning(t)

	err = cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	enqueueDirectly(t, served.state, name, "review")
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "review").ID, "succeeded")

	served.stop(t)

	var results []string
	for _, request := range fake.allRequests() {
		results = append(results, request.toolResults()...)
	}

	joined := strings.Join(results, "\n")
	if !strings.Contains(joined, "match:***") {
		t.Errorf("the tool result = %q, want the command to have matched and the value masked", joined)
	}

	if strings.Contains(joined, e2eSecretValue) {
		t.Error("the model was shown the agent's secret")
	}

	assertNotInDatabase(t, served.state, e2eSecretValue)
}

// Each place a secret would be silently dropped is a load error instead.
func TestSecretsAreRefusedWhereNothingWouldResolveThem(t *testing.T) {
	t.Parallel()

	const hosted = "source: {endpoint: http://127.0.0.1:1/v1/, model: m}\n"

	for _, tc := range []struct{ name, pipeline, want string }{
		{"cli agent", "agents:\n- name: a\n  source: {model: \"@claude/sonnet\"}\n  secrets: {T: ((ci:t))}\njobs:\n- name: j\n  plan:\n  - agent: a\n    messages: [hi]\n", "cli agent"},
		{"fix agent", "agents:\n- name: a\n  " + hosted + "  secrets: {T: ((ci:t))}\njobs:\n- name: j\n  plan:\n  - task: t\n    run: exit 1\n    fix: a\n", "fix agent never receives"},
		{"sub-agent", "agents:\n- name: a\n  description: d\n  " + hosted + "  secrets: {T: ((ci:t))}\n- name: b\n  " + hosted + "  tools: [{agent: a}]\njobs:\n- name: j\n  plan:\n  - agent: b\n    messages: [hi]\n", "only an agent step"},
		{"type env collision", "resource_types:\n- name: t\n  env: [TOKEN]\n  config: {check: 'echo []'}\nresources:\n- name: r\n  type: t\n  source: {}\n  secrets: {TOKEN: ((ci:t))}\njobs:\n- name: j\n  plan:\n  - get: r\n", "named in both env and secrets"},
		{"task env, step secrets", "tasks:\n- name: t\n  env: [TOK]\n  run: echo hi\njobs:\n- name: j\n  plan:\n  - task: t\n    secrets: {TOK: ((ci:t))}\n", "named in both env and secrets"},
		{"task secrets, step env", "tasks:\n- name: t\n  secrets: {TOK: ((ci:t))}\n  run: echo hi\njobs:\n- name: j\n  plan:\n  - task: t\n    env: [TOK]\n", "named in both env and secrets"},
		{"not an identifier", "jobs:\n- name: j\n  plan:\n  - task: t\n    secrets: {A-B: ((ci:t))}\n    run: echo hi\n", "shell variable name"},
		{"traceparent", "jobs:\n- name: j\n  plan:\n  - task: t\n    secrets: {TRACEPARENT: ((ci:t))}\n    run: echo hi\n", "TRACEPARENT"},
		{"get step", "resources:\n- name: r\n  type: cron\n  source: {schedule: '0 * * * *'}\njobs:\n- name: j\n  plan:\n  - get: r\n    secrets: {T: ((ci:t))}\n", "task and agent steps only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := pipelinePath(t, dir)
			writePipelineFile(t, path, tc.pipeline)

			err := cli.Run([]string{"validate", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// A resource's secrets reach both its poll, which runs outside any run, and its in: inside one; the poll is the case with no run to cache the lookup in.
func TestResourceSecretsReachItsCheckAndIn(t *testing.T) {
	dir := t.TempDir()
	expected := filepath.Join(dir, "expected")

	err := os.WriteFile(expected, []byte(e2eSecretValue), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	writePipelineFile(t, path, `
resource_types:
- name: guarded
  config:
    check: test "$TOKEN" = "$(cat `+expected+`)" && echo '[{"n":"1"}]'
    in: test "$TOKEN" = "$(cat `+expected+`)"

resources:
- name: feed
  type: guarded
  source: {}
  secrets:
    TOKEN: ((ci:gh_token))

jobs:
- name: consume
  plan:
  - get: feed
    trigger: true
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue}, "--interval", "200ms")
	defer served.stopIfRunning(t)

	err = cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "consume").ID, "succeeded")

	served.stop(t)

	assertNotInDatabase(t, served.state, e2eSecretValue)
}

// A failed poll's error is stored and shown on the resource's page; one that quotes the resource's credential must not keep it.
func TestAFailedChecksErrorIsScrubbed(t *testing.T) {
	logs := captureStderr(t)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	writePipelineFile(t, path, `
resource_types:
- name: leaky
  config:
    expr:
      check: fail("leaked " + env("TOKEN"))

resources:
- name: feed
  type: leaky
  source: {}
  secrets:
    TOKEN: ((ci:gh_token))

jobs:
- name: consume
  plan:
  - get: feed
    trigger: true
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue}, "--interval", "200ms")
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for !inDatabase(t, served.state, "leaked ***") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	served.stop(t)

	if !inDatabase(t, served.state, "leaked ***") {
		t.Fatal("no scrubbed check error was recorded")
	}

	assertNotInDatabase(t, served.state, e2eSecretValue)

	// The poll also logs the failure every interval; the daemon's log is as durable as its database.
	if strings.Contains(logs(), e2eSecretValue) {
		t.Error("the daemon log carries the secret from a failed check")
	}
}

// A sub-agent was granted nothing the parent step resolved: its tools run without the parent's secrets, the way a fix agent's do.
func TestSubAgentDoesNotInheritTheParentsSecrets(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "fake")

	fake := newFakeLLM(t,
		callsTool("helper", map[string]any{"request": "look"}),
		callsTool("run_shell", map[string]any{"command": `printf 'seen=[%s]' "$TOKEN"`}),
		says("child done"),
		says("parent done"),
	)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true

agents:
- name: helper
  description: looks things up
  source: {endpoint: `+fake.URL+`/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools: [run_shell]
- name: lead
  source: {endpoint: `+fake.URL+`/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools: [{agent: helper}]
  secrets:
    TOKEN: ((ci:gh_token))

jobs:
- name: delegate
  plan:
  - agent: lead
    messages: [delegate it]
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue})
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	enqueueDirectly(t, served.state, name, "delegate")
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "delegate").ID, "succeeded")

	served.stop(t)

	var results []string
	for _, request := range fake.allRequests() {
		results = append(results, request.toolResults()...)
	}

	if joined := strings.Join(results, "\n"); !strings.Contains(joined, "seen=[]") {
		t.Errorf("the sub-agent's run_shell result = %q, want it to have seen no TOKEN", joined)
	}
}

// A get that fails inside a run records its error as the step's and the job's outcome, which the browser shows; an error quoting the resource's credential must be masked there too.
func TestAFailedGetsErrorIsScrubbed(t *testing.T) {
	logs := captureStderr(t)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	writePipelineFile(t, path, `
resource_types:
- name: leaky
  config:
    expr:
      check: '[{n: "1"}]'
      in: fail("leaked " + env("TOKEN"))

resources:
- name: feed
  type: leaky
  source: {}
  secrets:
    TOKEN: ((ci:gh_token))

jobs:
- name: fetch
  plan:
  - get: feed
`)

	served := secretDaemon(t, dir, map[string]string{name + "/gh_token": e2eSecretValue})
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "ops", "pw")})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	enqueueDirectly(t, served.state, name, "fetch")
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "fetch").ID, "failed")

	served.stop(t)

	if !inDatabase(t, served.state, "leaked ***") {
		t.Error("the failed get's masked error was not recorded")
	}

	assertNotInDatabase(t, served.state, e2eSecretValue)

	if strings.Contains(logs(), e2eSecretValue) {
		t.Error("the daemon log carries the secret from a failed get")
	}
}

// writeVault lays items out as files under dir, the layout a 1Password vault with a folder per pipeline would have.
func writeVault(t *testing.T, dir string, items map[string]string) {
	t.Helper()

	for path, value := range items {
		file := filepath.Join(dir, filepath.FromSlash(path))

		err := os.MkdirAll(filepath.Dir(file), 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(file, []byte(value), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}

const e2eNpmValue = "npm-value-for-validate-3b1d"

// validateSecretsFixture is a pipeline whose npm_token sits in its own folder and whose gh_token falls through to the shared one, with the daemon's config file naming both paths.
func validateSecretsFixture(t *testing.T) (path, config, vault string) {
	t.Helper()

	dir := t.TempDir()
	path = pipelinePath(t, dir)
	name := cli.PipelineName(path)

	vault = filepath.Join(dir, "vault")
	writeVault(t, vault, map[string]string{
		name + "/npm_token": e2eNpmValue,
		"shared/gh_token":   e2eSecretValue,
	})

	config = filepath.Join(dir, "steps.yml")
	writeConfigFile(t, config, `
web:
  secret:
    source:
      ci:
        command: cat {{ .path | shellquote }}
        missing: exit_code == 1
        lookup:
        - path: `+vault+`/{{ .pipeline }}/{{ .name }}
        - path: `+vault+`/shared/{{ .name }}
`)

	writePipelineFile(t, path, `
resource_types:
- name: guarded
  config:
    check: echo '[{"n":"1"}]'
    in: "true"

resources:
- name: feed
  type: guarded
  source: {}
  secrets:
    GH_TOKEN: ((ci:gh_token))

jobs:
- name: build
  plan:
  - get: feed
    trigger: true
  - task: publish
    secrets:
      NPM_TOKEN: ((ci:npm_token))
    run: "true"
`)

	return path, config, vault
}

// steps validate --live --config <the daemon's file> resolves every reference the way the daemon would, names the path that answered and keeps the values to itself: the one way to learn a login has lapsed, or a name falls through to the shared folder, before a job is half paid for.
//
// Not t.Parallel(): captureStdout swaps os.Stdout.
func TestValidateLiveResolvesEverySecretAndNamesItsPath(t *testing.T) {
	path, config, vault := validateSecretsFixture(t)
	name := cli.PipelineName(path)

	var err error

	out := captureStdout(t, func() { err = cli.Run([]string{"validate", "--live", "--config", config, path}) })
	if err != nil {
		t.Fatalf("validate --live: %v", err)
	}

	for _, want := range []string{
		`secret ((ci:npm_token)) for job "build" is at ` + vault + "/" + name + "/npm_token",
		`secret ((ci:gh_token)) for resource "feed" in job "build" is at ` + vault + "/shared/gh_token",
		`secret ((ci:gh_token)) for resource "feed" when polled is at ` + vault + "/shared/gh_token",
		"2 secret(s) found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("validate --live printed:\n%s\nwant %q", out, want)
		}
	}

	for _, value := range []string{e2eSecretValue, e2eNpmValue} {
		if strings.Contains(out, value) {
			t.Errorf("validate --live printed a secret's value:\n%s", out)
		}
	}

	// --job narrows the probe to that job's references, as it does its models and servers.
	out = captureStdout(t, func() { err = cli.Run([]string{"validate", "--live", "--job", "build", "--config", config, path}) })
	if err != nil {
		t.Fatalf("validate --live --job build: %v", err)
	}

	if strings.Contains(out, "when polled") {
		t.Errorf("validate --live --job build resolved the poll's reference, which is no job's:\n%s", out)
	}
}

// A name no path has is a problem naming where it looked, not a half-vouched ok; and --config is the live depth's input: alone it binds nothing, and --live with references and no sources cannot vouch for them.
func TestValidateLiveRefusesWhatItCannotVouchFor(t *testing.T) {
	t.Parallel()

	path, config, vault := validateSecretsFixture(t)

	err := cli.Run([]string{"validate", "--config", config, path})
	if err == nil || !strings.Contains(err.Error(), "--live") {
		t.Fatalf("validate --config without --live = %v, want a refusal naming --live", err)
	}

	err = cli.Run([]string{"validate", "--live", path})
	if err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("validate --live with references and no --config = %v, want a refusal naming --config", err)
	}

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: publish
    secrets:
      TOKEN: ((ci:absent))
    run: "true"
`)

	err = cli.Run([]string{"validate", "--live", "--config", config, path})
	if err == nil || !strings.Contains(err.Error(), "((ci:absent))") || !strings.Contains(err.Error(), vault+"/shared/absent") {
		t.Fatalf("validate --live with a missing secret = %v, want a problem naming the reference and the paths tried", err)
	}

	// A --config that cannot be read is wrong before the pipeline has its first reference, not only after.
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: publish
    run: "true"
`)

	err = cli.Run([]string{"validate", "--live", "--config", filepath.Join(t.TempDir(), "absent.yml"), path})
	if err == nil || !strings.Contains(err.Error(), "absent.yml") {
		t.Fatalf("validate --live --config <missing file> on a pipeline without references = %v, want a refusal naming the file", err)
	}
}
