package e2e

// Not t.Parallel() anywhere here: runAndCapture swaps the process's streams, and the probe cache is process-wide.

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

const e2eModelKey = "model-key-from-the-vault-7c2e"

// keyOnlyInVault is a variable name nothing sets: the key these agents use exists in the vault alone.
const keyOnlyInVault = "STEPS_TEST_KEY_ONLY_IN_VAULT"

// probeOrWork answers a preflight probe, then one run_shell call, then stops: routed, since whether a probe was sent is the cache's business.
func probeOrWork(command string) func(capturedRequest) turn {
	return func(request capturedRequest) turn {
		switch {
		case len(request.toolResults()) > 0:
			return says("done")
		case request.userMessageContains("check the key"):
			return callsTool("run_shell", map[string]any{"command": command})
		default:
			return says("pong")
		}
	}
}

func assertEveryBearer(t *testing.T, model *fakeLLM, want string) {
	t.Helper()

	if model.requestCount() == 0 {
		t.Fatal("the model was never reached, so its credential proves nothing")
	}

	for i, request := range model.allRequests() {
		if request.Authorization != "Bearer "+want {
			t.Errorf("request %d carried Authorization %q, want the vault's value as its bearer token", i+1, request.Authorization)
		}
	}
}

// The spine: the variable is absent from the environment, the provider is sent the vault's value, the agent's own shell never holds it, and what would print it is masked.
func TestAgentModelKeyComesFromASecretSource(t *testing.T) {
	for _, command := range []string{"run", "test"} {
		t.Run(command, func(t *testing.T) {
			isolatePreflightPins(t)

			dir, path, config, vault := cliSecretsFixture(t, nil)
			name := cli.PipelineName(path)
			writeVault(t, vault, map[string]string{name + "/model_key": e2eModelKey + "\n"})

			// In a file rather than the pipeline: the pipeline's text is stored, so the value written there would be in the database by itself.
			expected := filepath.Join(dir, "expected")
			writePipelineFile(t, expected, e2eModelKey)

			model := newRoutedFakeLLM(t, probeOrWork(`printf 'env=[%s] file=%s' "$`+keyOnlyInVault+`" "$(cat `+expected+`)"`))

			writePipelineFile(t, path, `
agents:
- name: reviewer
  source:
    endpoint: `+model.URL+`/v1/
    model: test-model
    api_key_env: `+keyOnlyInVault+`
    secrets:
      `+keyOnlyInVault+`: ((ci:model_key))
  tools: [run_shell]

jobs:
- name: review
  plan:
  - agent: reviewer
    messages: [check the key]
`)

			printed, err := runAndCapture(t, command, "--config", config, path)
			if err != nil {
				t.Fatalf("steps %s --config: %v\n%s", command, err, printed)
			}

			assertEveryBearer(t, model, e2eModelKey)

			var results []string
			for _, request := range model.allRequests() {
				results = append(results, request.toolResults()...)
			}

			joined := strings.Join(results, "\n")
			if !strings.Contains(joined, "env=[] file=***") {
				t.Errorf("the agent's tool result = %q, want its shell without the variable and the printed key masked", joined)
			}

			if strings.Contains(joined, e2eModelKey) {
				t.Errorf("the model was sent its own key: %q", joined)
			}

			assertNotInDatabase(t, string(cli.StatePath(path, "")), e2eModelKey)
		})
	}
}

const e2eBackupKey = "backup-key-from-the-vault-4d91"

// A fallback carries its own secrets: map, resolved when the cascade reaches it: before the run from the probe, and mid-run when the primary gives up.
func TestAFallbackSourceUsesItsOwnSecretBackedKey(t *testing.T) {
	for _, tc := range []struct {
		name, defaults string
		primary        []turn
		backup         []turn
	}{
		{"chosen by preflight", "", []turn{failsWith(http.StatusInternalServerError), failsWith(http.StatusInternalServerError), failsWith(http.StatusInternalServerError)}, []turn{says("probe ok"), says("done")}},
		{"reached mid-run", "defaults:\n  preflight:\n    disabled: true\n", []turn{failsWith(http.StatusInternalServerError)}, []turn{says("done")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatePreflightPins(t)

			_, path, config, vault := cliSecretsFixture(t, nil)
			name := cli.PipelineName(path)
			writeVault(t, vault, map[string]string{name + "/model_key": e2eModelKey, "shared/backup_key": e2eBackupKey})

			primary := newFakeLLM(t, tc.primary...)
			backup := newFakeLLM(t, tc.backup...)

			writePipelineFile(t, path, tc.defaults+`
agents:
- name: writer
  source:
    endpoint: `+primary.URL+`/v1/
    model: primary-model
    api_key_env: `+keyOnlyInVault+`
    secrets:
      `+keyOnlyInVault+`: ((ci:model_key))
  fallback:
  - source:
      endpoint: `+backup.URL+`/v1/
      model: backup-model
      api_key_env: `+keyOnlyInVault+`
      secrets:
        `+keyOnlyInVault+`: ((ci:backup_key))

jobs:
- name: publish
  plan:
  - agent: writer
    inputs: []
    attempts: 1
    messages: [Write something.]
`)

			printed, err := runAndCapture(t, "run", "--config", config, path)
			if err != nil {
				t.Fatalf("run --config: %v\n%s", err, printed)
			}

			assertEveryBearer(t, primary, e2eModelKey)
			assertEveryBearer(t, backup, e2eBackupKey)
		})
	}
}

// A sub-agent and a fix agent are hosted agents too: each needs a key, and the key never reaches a tool, so neither is refused the way an agent's own secrets: is.
func TestSubAgentsAndFixAgentsUseASecretBackedKey(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "fake")

	_, path, config, vault := cliSecretsFixture(t, nil)
	name := cli.PipelineName(path)
	writeVault(t, vault, map[string]string{name + "/model_key": e2eModelKey})

	lead := newFakeLLM(t, callsTool("helper", map[string]any{"request": "look"}), says("parent done"))
	keyed := newRoutedFakeLLM(t, func(capturedRequest) turn { return says("done") })

	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true

agents:
- name: helper
  description: looks things up
  source: &keyed
    endpoint: `+keyed.URL+`/v1/
    model: test-model
    api_key_env: `+keyOnlyInVault+`
    secrets:
      `+keyOnlyInVault+`: ((ci:model_key))
- name: fixer
  source: *keyed
- name: lead
  source: {endpoint: `+lead.URL+`/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools: [{agent: helper}]

jobs:
- name: delegate
  plan:
  - agent: lead
    messages: [delegate it]
  - task: broken
    run: exit 1
    fix: fixer
`)

	_, _ = runAndCapture(t, "run", "--config", config, path)

	// The delegation and the repair: two conversations, each opened with the vault's key.
	if got := keyed.requestCount(); got < 2 {
		t.Fatalf("the secret-backed model served %d request(s), want the sub-agent's and the fix agent's", got)
	}

	assertEveryBearer(t, keyed, e2eModelKey)
}

// Each place a source's secrets: would be read by nothing, or by something that must not hold it, is a load error.
func TestSourceSecretsAreRefusedAtLoad(t *testing.T) {
	t.Parallel()

	const job = "jobs:\n- name: j\n  plan:\n  - agent: a\n    messages: [hi]\n"

	for _, tc := range []struct{ name, pipeline, want string }{
		{"no api_key_env", "agents:\n- name: a\n  source: {endpoint: http://127.0.0.1:1/v1/, model: m, secrets: {K: ((ci:k))}}\n" + job, "needs api_key_env"},
		{"api_key_env names another", "agents:\n- name: a\n  source: {model: openrouter/m, secrets: {K: ((ci:k))}}\n" + job, "OPENROUTER_API_KEY is not a key"},
		{"an extra key", "agents:\n- name: a\n  source: {model: openrouter/m, api_key_env: K, secrets: {K: ((ci:k)), GH_TOKEN: ((ci:t))}}\n" + job, "nothing would read GH_TOKEN"},
		{"cli agent", "agents:\n- name: a\n  source: {model: \"@claude/sonnet\", api_key_env: K, secrets: {K: ((ci:k))}}\n" + job, "CLI subprocess's environment"},
		{"behind an export", "agents:\n- name: a\n  description: d\n  source: {model: openrouter/m, secrets: {OPENROUTER_API_KEY: ((ci:k))}}\nmcp_exports:\n- {name: x, access: 'true', tools: [{agent: a}]}\n" + job, "no run to resolve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := pipelinePath(t, t.TempDir())
			writePipelineFile(t, path, tc.pipeline)

			err := cli.Run([]string{"validate", "--syntax-only", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// The daemon path: set does not ask the environment for a key the vault holds, a run on the daemon sends it, and a source the daemon lacks is refused at set.
func TestPipelineSetAcceptsAModelKeyHeldOnlyAsASecret(t *testing.T) {
	isolatePreflightPins(t)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	name := cli.PipelineName(path)

	model := newRoutedFakeLLM(t, probeOrWork("true"))

	pipeline := func(source string) string {
		return `
agents:
- name: reviewer
  source:
    endpoint: ` + model.URL + `/v1/
    model: test-model
    api_key_env: ` + keyOnlyInVault + `
    secrets:
      ` + keyOnlyInVault + `: ((` + source + `:model_key))
  tools: [run_shell]

jobs:
- name: review
  plan:
  - agent: reviewer
    messages: [check the key]
`
	}

	writePipelineFile(t, path, pipeline("ci"))

	served := secretDaemon(t, dir, map[string]string{name + "/model_key": e2eModelKey})
	defer served.stopIfRunning(t)

	target := authTarget(served.addr, "ops", "pw")

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", target})
	if err != nil {
		t.Fatalf("pipeline set: %v", err)
	}

	enqueueDirectly(t, served.state, name, "review")
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "review").ID, "succeeded")

	writePipelineFile(t, path, pipeline("vault"))

	err = cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", target})
	if err == nil || !strings.Contains(err.Error(), `"vault"`) {
		t.Errorf("pipeline set = %v, want a refusal naming the unknown source", err)
	}

	served.stop(t)

	assertEveryBearer(t, model, e2eModelKey)
	assertNotInDatabase(t, served.state, e2eModelKey)
}

// validate --live probes the model with the key the daemon would use and says where the reference was found, never what it holds.
func TestValidateLiveResolvesAModelKeyAndNamesItsPath(t *testing.T) {
	isolatePreflightPins(t)

	_, path, config, vault := cliSecretsFixture(t, nil)
	writeVault(t, vault, map[string]string{"shared/model_key": e2eModelKey})

	model := newRoutedFakeLLM(t, func(capturedRequest) turn { return says("pong") })

	writePipelineFile(t, path, `
agents:
- name: reviewer
  source:
    endpoint: `+model.URL+`/v1/
    model: test-model
    api_key_env: `+keyOnlyInVault+`
    secrets:
      `+keyOnlyInVault+`: ((ci:model_key))

jobs:
- name: review
  plan:
  - agent: reviewer
    messages: [hi]
`)

	// Without the sources, the reference is refused before anything is dialled.
	err := cli.Run([]string{"validate", "--live", path})
	if err == nil || !strings.Contains(err.Error(), "--config") || model.requestCount() != 0 {
		t.Fatalf("validate --live without --config = %v after %d request(s), want a refusal naming --config and no probe", err, model.requestCount())
	}

	out := captureStdout(t, func() { err = cli.Run([]string{"validate", "--live", "--config", config, path}) })
	if err != nil {
		t.Fatalf("validate --live: %v", err)
	}

	for _, want := range []string{`secret ((ci:model_key)) for job "review" is at ` + vault + "/shared/model_key", "1 secret(s) found"} {
		if !strings.Contains(out, want) {
			t.Errorf("validate --live printed:\n%s\nwant %q", out, want)
		}
	}

	if strings.Contains(out, e2eModelKey) {
		t.Errorf("validate --live printed the key:\n%s", out)
	}

	assertEveryBearer(t, model, e2eModelKey)

	// The file-and-machine depth never asks the environment for it.
	err = cli.Run([]string{"validate", path})
	if err != nil {
		t.Errorf("validate = %v, want a key held as a secret not reported unset", err)
	}
}
