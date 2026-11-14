package e2e

// Not t.Parallel() anywhere here: captureStdout and captureStderr swap the process's streams.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// cliSecretsFixture is a vault with one folder per pipeline plus a shared one, and the daemon's config file naming both paths, with the keys a daemon owns beside the secret: section that a local run must leave alone.
func cliSecretsFixture(t *testing.T, items map[string]string) (dir, path, config, vault string) {
	t.Helper()

	dir = t.TempDir()
	path = pipelinePath(t, dir)
	vault = filepath.Join(dir, "vault")
	writeVault(t, vault, items)

	config = filepath.Join(dir, "steps.yml")
	writeConfigFile(t, config, `
web:
  listen: 127.0.0.1:1
  local-users-file: `+filepath.Join(dir, "absent-users")+`
  secret:
    source:
      ci:
        command: cat {{ .path | shellquote }}
        missing: exit_code == 1
        lookup:
        - path: `+vault+`/{{ .pipeline }}/{{ .name }}
        - path: `+vault+`/shared/{{ .name }}
`)

	return dir, path, config, vault
}

// runAndCapture is what an operator at the terminal sees: both streams, and the error main would print.
func runAndCapture(t *testing.T, args ...string) (printed string, err error) {
	t.Helper()

	stderr := captureStderr(t)

	out := captureStdout(t, func() { err = cli.Run(args) })

	printed = out + stderr()
	if err != nil {
		printed += err.Error()

		return printed, fmt.Errorf("%w", err)
	}

	return printed, nil
}

// The step sees the variable; what it printed, whole or in two pieces, on either stream, is masked in the terminal and in the recorded output alike, and the value never reaches the database.
func TestRunConfigResolvesSecretsAndScrubsTheTerminal(t *testing.T) {
	for _, progress := range []string{"plain", "tty"} {
		t.Run(progress, func(t *testing.T) {
			dir, path, config, _ := cliSecretsFixture(t, nil)
			name := cli.PipelineName(path)
			writeVault(t, filepath.Join(dir, "vault"), map[string]string{name + "/gh_token": e2eSecretValue + "\n"})

			// The expected value sits in a file: the pipeline's text is stored, so writing the value there would put it in the database by itself.
			expected := filepath.Join(dir, "expected")
			writePipelineFile(t, expected, e2eSecretValue)

			writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: leak
    secrets:
      TOKEN: ((ci:gh_token))
    run: |
      test "$TOKEN" = "$(cat `+expected+`)"
      head=${TOKEN%??????????}
      printf 'token=%s' "$head"
      sleep 0.2
      printf '%s done\n' "${TOKEN#"$head"}"
      echo "err=$TOKEN" >&2
`)

			printed, err := runAndCapture(t, "run", "--config", config, "--progress", progress, path)
			if err != nil {
				t.Fatalf("run --config: %v\n%s", err, printed)
			}

			if strings.Contains(printed, e2eSecretValue) || strings.Contains(printed, e2eSecretValue[:10]) {
				t.Errorf("the terminal was shown the secret:\n%s", printed)
			}

			// The live view tails a partial line apart from the other stream's, so only the plain lines read whole.
			if !strings.Contains(printed, "*** done") || !strings.Contains(printed, "err=***") || (progress == "plain" && !strings.Contains(printed, "token=*** done")) {
				t.Errorf("the terminal lost the output instead of masking it:\n%s", printed)
			}

			state := string(cli.StatePath(path, ""))

			assertNotInDatabase(t, state, e2eSecretValue)
			assertNotInDatabase(t, state, e2eSecretValue[:10])

			if !inDatabase(t, state, "token=*** done") {
				t.Error("the recorded output does not read token=*** done")
			}
		})
	}
}

// A reference falls through the lookup paths as it does on the daemon, and the pipeline name the paths are scoped on is the one `steps runs -p` calls the file.
func TestRunConfigLookupFallsThroughAndMissingStops(t *testing.T) {
	_, path, config, vault := cliSecretsFixture(t, map[string]string{"shared/gh_token": e2eSecretValue})
	name := cli.PipelineName(path)

	writeVault(t, vault, map[string]string{name + "/npm_token": e2eNpmValue})

	expected := filepath.Join(filepath.Dir(path), "expected")
	writePipelineFile(t, expected, e2eSecretValue+e2eNpmValue)

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: both
    secrets:
      GH: ((ci:gh_token))
      NPM: ((ci:npm_token))
    run: test "$GH$NPM" = "$(cat `+expected+`)"
`)

	printed, err := runAndCapture(t, "run", "--config", config, path)
	if err != nil {
		t.Fatalf("run --config: %v\n%s", err, printed)
	}

	// A name no path has names where it looked.
	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: t\n    secrets: {T: ((ci:absent))}\n    run: \"true\"\n")

	_, err = runAndCapture(t, "run", "--config", config, "--force", path)
	if err == nil || !strings.Contains(err.Error(), vault+"/shared/absent") {
		t.Fatalf("run with a secret no path has = %v, want an error naming the paths tried", err)
	}

	// Only "missing" moves on: exit 3 is a failure, and the shared path that has the value is never asked.
	failing := filepath.Join(filepath.Dir(path), "failing.yml")
	writeConfigFile(t, failing, `
web:
  secret:
    source:
      ci:
        command: test -e {{ .path | shellquote }} || exit 3; cat {{ .path | shellquote }}
        missing: exit_code == 1
        lookup:
        - path: `+vault+`/{{ .pipeline }}/{{ .name }}
        - path: `+vault+`/shared/{{ .name }}
`)

	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: t\n    secrets: {T: ((ci:gh_token))}\n    run: \"true\"\n")

	_, err = runAndCapture(t, "run", "--config", failing, "--force", path)
	if err == nil || !strings.Contains(err.Error(), "exit") {
		t.Fatalf("run with a source failing with exit 3 = %v, want the failure, not the shared fallback", err)
	}
}

// A source the file does not define is found before the first step, not by the step that needs it.
func TestRunAndTestConfigRefuseAnUndefinedSourceBeforeAnyStep(t *testing.T) {
	dir, path, config, _ := cliSecretsFixture(t, nil)
	marker := filepath.Join(dir, "ran")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: first
    run: touch `+marker+`
  - task: second
    secrets: {TOKEN: ((vault:gh_token))}
    run: "true"
`)

	for _, command := range []string{"run", "test"} {
		_, err := runAndCapture(t, command, "--config", config, path)
		if err == nil || !strings.Contains(err.Error(), `"vault"`) {
			t.Errorf("steps %s = %v, want a refusal naming the undefined source", command, err)
		}

		assertNoFile(t, marker)
	}
}

// The file is held to the daemon's privilege checks: it names commands this user runs.
func TestRunConfigRefusesAWritableConfigFile(t *testing.T) {
	_, path, config, _ := cliSecretsFixture(t, nil)
	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: check\n    secrets: {TOKEN: ((ci:gh_token))}\n    run: echo hi\n")

	err := os.Chmod(config, 0o666) //nolint:gosec // the point of the test
	if err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{"run", "test"} {
		_, err = runAndCapture(t, command, "--config", config, path)
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Errorf("steps %s with a group-writable config = %v, want a refusal", command, err)
		}
	}
}

// steps test is the same resolution, and its own FAIL line and error are terminal output too.
func TestTestConfigResolvesAndScrubsAnAssertionFailure(t *testing.T) {
	dir, path, config, _ := cliSecretsFixture(t, nil)
	name := cli.PipelineName(path)
	writeVault(t, filepath.Join(dir, "vault"), map[string]string{name + "/gh_token": e2eSecretValue})

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: leak
    secrets:
      TOKEN: ((ci:gh_token))
    run: echo "token=$TOKEN"
    assert:
      stdout: never-printed
`)

	printed, err := runAndCapture(t, "test", "--config", config, path)
	if err == nil {
		t.Fatalf("test passed, want the assertion to fail:\n%s", printed)
	}

	if strings.Contains(printed, e2eSecretValue) {
		t.Errorf("the terminal was shown the secret:\n%s", printed)
	}

	// The passing run: the same pipeline asserting what the masked output reads.
	writePipelineFile(t, path, strings.Replace(readFileString(t, path), "never-printed", "token=***", 1))

	printed, err = runAndCapture(t, "test", "--config", config, path)
	if err != nil {
		t.Fatalf("test --config: %v\n%s", err, printed)
	}

	if !strings.Contains(printed, "PASS build") || strings.Contains(printed, e2eSecretValue) {
		t.Errorf("test printed:\n%s", printed)
	}
}

// --resume and --replay re-resolve what the step they run needs: the reason this flag exists.
func TestRunConfigResumeAndReplayResolveTheSecret(t *testing.T) {
	dir, path, config, _ := cliSecretsFixture(t, nil)
	name := cli.PipelineName(path)
	writeVault(t, filepath.Join(dir, "vault"), map[string]string{name + "/gh_token": e2eSecretValue})

	expected := filepath.Join(dir, "expected")
	writePipelineFile(t, expected, e2eSecretValue)

	marker := filepath.Join(dir, "fixed")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: first
    run: echo first
  - task: fragile
    secrets: {TOKEN: ((ci:gh_token))}
    run: |
      test "$TOKEN" = "$(cat `+expected+`)"
      test -f `+marker+`
`)

	printed, err := runAndCapture(t, "run", "--config", config, path)
	if err == nil {
		t.Fatalf("the first run passed, want the fragile step to fail:\n%s", printed)
	}

	id := resumeID(t, printed)

	writePipelineFile(t, marker, "")

	printed, err = runAndCapture(t, "run", "--config", config, "--resume", id, path)
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, printed)
	}

	printed, err = runAndCapture(t, "run", "--config", config, "--replay", id, "--from", "fragile", path)
	if err != nil {
		t.Fatalf("replay: %v\n%s", err, printed)
	}
}

// An agent's tools get the variable, and the model is sent the masked result; a fix agent gets neither the variable nor the assertion's raw output.
func TestRunConfigAgentToolResultsAndFixInputAreScrubbed(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "fake")

	dir, path, config, _ := cliSecretsFixture(t, nil)
	name := cli.PipelineName(path)
	writeVault(t, filepath.Join(dir, "vault"), map[string]string{name + "/gh_token": e2eSecretValue})

	reviewer := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": `printf 'match:%s' "$TOKEN"`}),
		says("done"),
	)

	fixer := newRoutedFakeLLM(t, func(request capturedRequest) turn {
		if len(request.toolResults()) > 0 {
			return says("done")
		}

		return callsTool("run_shell", map[string]any{"command": `printf 'seen=[%s]' "$TOKEN"`})
	})

	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source:
    endpoint: `+reviewer.URL+`/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
  secrets:
    TOKEN: ((ci:gh_token))
- name: fixer
  source:
    endpoint: `+fixer.URL+`/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

jobs:
- name: review
  plan:
  - agent: reviewer
    messages: [check the token]
  - task: leak
    secrets: {TOKEN: ((ci:gh_token))}
    run: echo "leaked=$TOKEN"
    assert:
      stdout: never-printed
    fix: fixer
`)

	_, _ = runAndCapture(t, "run", "--config", config, path)

	var results []string
	for _, request := range reviewer.allRequests() {
		results = append(results, request.toolResults()...)
	}

	joined := strings.Join(results, "\n")
	if !strings.Contains(joined, "match:***") || strings.Contains(joined, e2eSecretValue) {
		t.Errorf("the agent's tool result = %q, want it to have matched and be masked", joined)
	}

	if fixer.requestCount() == 0 {
		t.Fatal("the fix agent was never reached, so its input proves nothing")
	}

	assertFixAgentSawNoSecret(t, fixer)
}

// An agent tool streams live past recordOutput, so its terminal copy needs masking of its own (#255), whole or split across two writes.
func TestRunConfigAgentToolStreamIsScrubbedOnTheTerminal(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "fake")

	for _, progress := range []string{"plain", "tty"} {
		t.Run(progress, func(t *testing.T) {
			dir, path, config, _ := cliSecretsFixture(t, nil)
			writeVault(t, filepath.Join(dir, "vault"), map[string]string{cli.PipelineName(path) + "/gh_token": e2eSecretValue})

			fake := newFakeLLM(t,
				callsTool("run_shell", map[string]any{"command": `head=${TOKEN%??????????}; printf 'tok=%s' "$head"; sleep 0.2; printf '%s done\n' "${TOKEN#"$head"}"; echo "err=$TOKEN" >&2`}),
				says("done"),
			)

			writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true

agents:
- name: hand
  source:
    endpoint: `+fake.URL+`/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
  secrets:
    TOKEN: ((ci:gh_token))

jobs:
- name: build
  plan:
  - agent: hand
    messages: [go]
`)

			printed, err := runAndCapture(t, "run", "--config", config, "--progress", progress, path)
			if err != nil {
				t.Fatalf("run --config: %v\n%s", err, printed)
			}

			if strings.Contains(printed, e2eSecretValue) || strings.Contains(printed, e2eSecretValue[:10]) {
				t.Errorf("the terminal was shown the secret:\n%s", printed)
			}

			if !strings.Contains(printed, "err=***") || !strings.Contains(printed, "*** done") {
				t.Errorf("the terminal lost the tool's output instead of masking it:\n%s", printed)
			}
		})
	}
}

func assertFixAgentSawNoSecret(t *testing.T, fixer *fakeLLM) {
	t.Helper()

	var results []string

	for _, request := range fixer.allRequests() {
		for _, msg := range request.Messages {
			if strings.Contains(msg.Content, e2eSecretValue) {
				t.Errorf("the fix agent was shown the secret: %q", msg.Content)
			}
		}

		results = append(results, request.toolResults()...)
	}

	seen := strings.Join(results, "\n")
	if !strings.Contains(seen, "seen=[]") {
		t.Errorf("the fix agent's run_shell result = %q, want it to have seen no TOKEN", seen)
	}
}

// A resource's check and in: resolve the same way, and what in: prints on either stream, or its failed check's error, is as masked on the terminal as a task's.
func TestRunConfigResourceSecretsAreResolvedAndScrubbed(t *testing.T) {
	dir, path, config, _ := cliSecretsFixture(t, nil)
	name := cli.PipelineName(path)
	writeVault(t, filepath.Join(dir, "vault"), map[string]string{name + "/gh_token": e2eSecretValue})

	expected := filepath.Join(dir, "expected")
	writePipelineFile(t, expected, e2eSecretValue)

	writePipelineFile(t, path, `
resource_types:
- name: guarded
  config:
    check: test "$TOKEN" = "$(cat `+expected+`)" && echo '[{"n":"1"}]'
    in: |
      test "$TOKEN" = "$(cat `+expected+`)"
      echo "in-err=$TOKEN" >&2
      echo "in-out=$TOKEN"
- name: leaky
  config:
    expr:
      check: fail("leaked " + env("TOKEN"))

resources:
- name: feed
  type: guarded
  source: {}
  secrets:
    TOKEN: ((ci:gh_token))
- name: broken
  type: leaky
  source: {}
  secrets:
    TOKEN: ((ci:gh_token))

jobs:
- name: consume
  plan:
  - get: feed
- name: poll
  plan:
  - get: broken
`)

	printed, err := runAndCapture(t, "run", "--config", config, "--job", "consume", path)
	if err != nil {
		t.Fatalf("run --config: %v\n%s", err, printed)
	}

	if strings.Contains(printed, e2eSecretValue) || !strings.Contains(printed, "in-err=***") {
		t.Errorf("the terminal after a get:\n%s", printed)
	}

	printed, err = runAndCapture(t, "run", "--config", config, "--job", "poll", path)
	if err == nil || strings.Contains(printed, e2eSecretValue) || !strings.Contains(printed, "leaked ***") {
		t.Errorf("a failed check = %v, want an error naming the leak masked:\n%s", err, printed)
	}

	assertNotInDatabase(t, string(cli.StatePath(path, "")), e2eSecretValue)
}

// A step keys on the reference, never the value: a rotated credential re-runs nothing.
func TestRunConfigNeverHashesTheValue(t *testing.T) {
	_, path, config, vault := cliSecretsFixture(t, nil)
	name := cli.PipelineName(path)
	writeVault(t, vault, map[string]string{name + "/gh_token": e2eSecretValue})

	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: work\n    secrets: {TOKEN: ((ci:gh_token))}\n    run: echo ran-$RANDOM\n")

	first, err := runAndCapture(t, "run", "--config", config, path)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, first)
	}

	writeVault(t, vault, map[string]string{name + "/gh_token": e2eNpmValue})

	second, err := runAndCapture(t, "run", "--config", config, path)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, second)
	}

	if strings.Contains(second, "ran-") {
		t.Errorf("a rotated credential re-ran the step:\n%s", second)
	}
}
