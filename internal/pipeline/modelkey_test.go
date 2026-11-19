package pipeline

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/secret"
	"github.com/jtarchie/steps/internal/shell"
)

// bearerRecorder answers every request as a model would and keeps the credential each one carried.
type bearerRecorder struct {
	mu      sync.Mutex
	bearers []string
	*httptest.Server
}

func newBearerRecorder(t *testing.T) *bearerRecorder {
	t.Helper()

	recorder := &bearerRecorder{}
	recorder.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.mu.Lock()
		recorder.bearers = append(recorder.bearers, r.Header.Get("Authorization"))
		recorder.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"p","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(recorder.Close)

	return recorder
}

func (b *bearerRecorder) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.bearers)
}

// vaultSources is a source whose path names the job, so a test can tell which job a lookup was made for.
func vaultSources(t *testing.T, items map[string]string) context.Context {
	t.Helper()

	vault := t.TempDir()

	for path, value := range items {
		file := filepath.Join(vault, path)

		err := os.MkdirAll(filepath.Dir(file), 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(file, []byte(value), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	resolver, err := secret.Parse(fmt.Appendf(nil, "source:\n  ci:\n    command: cat {{ .path | shellquote }}\n    lookup:\n    - path: %s/{{ .pipeline }}/job-{{ .job }}/{{ .name }}\n", vault))
	if err != nil {
		t.Fatal(err)
	}

	return WithSecrets(t.Context(), resolver)
}

func keyedPipeline(t *testing.T, name, endpoint string) *config.Config {
	t.Helper()

	cfg, err := config.Parse(fmt.Appendf(nil, `
agents:
- name: reviewer
  source:
    endpoint: %s/v1/
    model: m
    api_key_env: MODEL_KEY
    secrets:
      MODEL_KEY: ((ci:model_key))
jobs:
- name: review
  plan:
  - agent: reviewer
    messages: [hi]
`, endpoint), name, nil)
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

// The poller's preflight has a memo and no job, so a model's key is looked up there with the job empty.
func TestPollPreflightResolvesAModelKeyWithAnEmptyJob(t *testing.T) {
	ResetPreflightCache()
	t.Cleanup(ResetPreflightCache)

	model := newBearerRecorder(t)
	ctx := vaultSources(t, map[string]string{
		"alpha/job-/model_key":       "key-for-the-poll-of-alpha",
		"alpha/job-review/model_key": "key-for-the-review-job",
	})

	problems := PreflightPipeline(WithPollSecrets(ctx, "alpha", time.Minute), keyedPipeline(t, "alpha", model.URL), nil)
	if len(problems) != 0 {
		t.Fatalf("preflight = %v, want the key found under the empty job", problems)
	}

	if got := model.seen(); !slices.Equal(got, []string{"Bearer key-for-the-poll-of-alpha"}) {
		t.Errorf("the probe carried %q, want the value at the empty job's path", got)
	}
}

// One endpoint, one model, one variable name, two pipelines: each is probed with its own credential, and one's missing key is not vouched for by the other's answer.
func TestAModelProbeIsNotSharedAcrossPipelinesWithSecretBackedKeys(t *testing.T) {
	ResetPreflightCache()
	t.Cleanup(ResetPreflightCache)

	model := newBearerRecorder(t)
	ctx := vaultSources(t, map[string]string{
		"alpha/job-/model_key": "key-of-pipeline-alpha",
		"beta/job-/model_key":  "key-of-pipeline-beta",
	})

	for _, name := range []string{"alpha", "beta", "alpha"} {
		problems := PreflightPipeline(WithPollSecrets(ctx, name, time.Minute), keyedPipeline(t, name, model.URL), nil)
		if len(problems) != 0 {
			t.Fatalf("preflight of %s = %v", name, problems)
		}
	}

	if got := model.seen(); !slices.Equal(got, []string{"Bearer key-of-pipeline-alpha", "Bearer key-of-pipeline-beta"}) {
		t.Errorf("the probes carried %q, want one per pipeline, each with its own key, and the repeat answered from the cache", got)
	}

	problems := PreflightPipeline(WithPollSecrets(ctx, "gamma", time.Minute), keyedPipeline(t, "gamma", model.URL), nil)
	if len(problems) != 1 || problems[0].Transient {
		t.Fatalf("preflight of a pipeline with no key in the vault = %+v, want one terminal problem, not another pipeline's cached answer", problems)
	}

	// A failed lookup is not remembered: once the vault has the key, the next pass finds it.
	problems = PreflightPipeline(WithPollSecrets(vaultSources(t, map[string]string{"gamma/job-/model_key": "key-of-pipeline-gamma"}), "gamma", time.Minute), keyedPipeline(t, "gamma", model.URL), nil)
	if len(problems) != 0 {
		t.Errorf("preflight after the key appeared = %v, want the lookup retried rather than its failure cached", problems)
	}
}

func jobScopedMCPPipeline(t *testing.T, model, mcp string) *config.Config {
	t.Helper()

	cfg, err := config.Parse(fmt.Appendf(nil, `
mcp_servers:
- name: tracker
  endpoint: %s/mcp
  auth:
    type: bearer
    api_key_env: TRACKER_TOKEN
    secrets:
      TRACKER_TOKEN: ((ci:tracker_token))
agents:
- name: reader
  source:
    endpoint: %s/v1/
    model: m
    api_key_env: MODEL_KEY
    secrets:
      MODEL_KEY: ((ci:model_key))
  tools:
  - mcp: tracker
    tool: read_items
jobs:
- name: good
  plan:
  - agent: reader
    messages: [hi]
- name: bad
  plan:
  - agent: reader
    messages: [hi]
`, mcp, model), "alpha", nil)
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

// A lookup path naming the job resolves one server's token per job, so one job's probe verdict must not answer for another's.
func TestAnMCPProbeIsNotSharedAcrossJobsWithSecretBackedTokens(t *testing.T) {
	ResetPreflightCache()
	t.Cleanup(ResetPreflightCache)

	model := newBearerRecorder(t)
	server := newBearerRecorder(t)
	ctx := WithProbeSecrets(vaultSources(t, map[string]string{
		"alpha/job-good/model_key":     "model-key",
		"alpha/job-bad/model_key":      "model-key",
		"alpha/job-good/tracker_token": "token-of-good",
		"alpha/job-bad/tracker_token":  "token-of-bad",
	}), "alpha")
	cfg := jobScopedMCPPipeline(t, model.URL, server.URL)

	for _, name := range []string{"bad", "good"} {
		job, err := cfg.FindJob(name)
		if err != nil {
			t.Fatal(err)
		}

		Preflight(shell.WithBuildMetadata(ctx, shell.BuildMetadata{PipelineName: "alpha", JobName: name}), cfg, job)
	}

	seen := server.seen()
	if !slices.Contains(seen, "Bearer token-of-bad") || !slices.Contains(seen, "Bearer token-of-good") {
		t.Errorf("the server was shown %q, want each job's own token rather than the second answered from the first's cached verdict", seen)
	}
}
