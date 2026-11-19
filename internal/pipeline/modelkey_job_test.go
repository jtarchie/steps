package pipeline

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// One pipeline, one model, one variable name, three jobs whose lookup path names .job: each job is probed with its own key, and the job with none is not vouched for by another job's cached answer.
func TestAModelProbeIsNotSharedAcrossJobsWithSecretBackedKeys(t *testing.T) {
	ResetPreflightCache()
	t.Cleanup(ResetPreflightCache)

	model := newBearerRecorder(t)
	ctx := WithProbeSecrets(vaultSources(t, map[string]string{
		"alpha/job-a/model_key": "key-of-job-a",
		"alpha/job-b/model_key": "key-of-job-b",
	}), "alpha")

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
- name: a
  plan: [{agent: reviewer, messages: [hi]}]
- name: b
  plan: [{agent: reviewer, messages: [hi]}]
- name: c
  plan: [{agent: reviewer, messages: [hi]}]
`, model.URL), "alpha", nil)
	if err != nil {
		t.Fatal(err)
	}

	preflightJob := func(name string) []config.Problem {
		job, err := cfg.FindJob(name)
		if err != nil {
			t.Fatal(err)
		}

		return Preflight(shell.WithBuildMetadata(ctx, shell.BuildMetadata{PipelineName: "alpha", JobName: name}), cfg, job)
	}

	for _, name := range []string{"a", "b", "a"} {
		problems := preflightJob(name)
		if len(problems) != 0 {
			t.Fatalf("preflight of job %s = %v", name, problems)
		}
	}

	if got := model.seen(); !slices.Equal(got, []string{"Bearer key-of-job-a", "Bearer key-of-job-b"}) {
		t.Errorf("the probes carried %q, want one per job, each with its own key, and the repeat answered from the cache", got)
	}

	problems := preflightJob("c")
	if len(problems) != 1 || problems[0].Transient {
		t.Errorf("preflight of a job with no key in the vault = %+v, want one terminal problem, not another job's cached answer", problems)
	}
}
