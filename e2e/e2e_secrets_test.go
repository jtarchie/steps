package e2e

// Secrets cross every seam steps has — config file → resolver, pipeline YAML → reference, run → step environment, and the store that must never see the value — so these start at the daemon and end at the database file's bytes.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/testsshd"
)

const e2eSecretValue = "s3cr3t-value-for-e2e-0f9a"

// secretDaemon serves a daemon whose ci source reads <vault>/<pipeline>/<name>, the layout a 1Password vault per pipeline would have.
func secretDaemon(t *testing.T, dir string, items map[string]string, args ...string) *webProcess {
	t.Helper()

	vault := filepath.Join(dir, "vault")

	for path, value := range items {
		file := filepath.Join(vault, filepath.FromSlash(path))

		err := os.MkdirAll(filepath.Dir(file), 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(file, []byte(value), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

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
