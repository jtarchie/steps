package e2e

// A pipeline on a real DigitalOcean droplet, through the real CLI: opt-in, after `hack/do-fixture.sh up` and with DIGITALOCEAN_ACCESS_TOKEN exported; it creates one droplet and deletes it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealDOPipelineStepRunsOnALaunchedDroplet(t *testing.T) {
	region := os.Getenv("STEPS_TEST_DO_REGION")
	if region == "" || os.Getenv("DIGITALOCEAN_ACCESS_TOKEN") == "" {
		t.Skip("no DigitalOcean fixture — run hack/do-fixture.sh up, export what it prints and DIGITALOCEAN_ACCESS_TOKEN")
	}

	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt
  - task: remote
    tags: [do]
    image: public.ecr.aws/docker/library/alpine:3
    inputs: [data]
    outputs: [model]
    run: |
      cat data/seed.txt > model/report.txt
      uname -m >> model/report.txt
      echo "$STEPS_WORKER" >> model/report.txt
  - task: publish
    inputs: [model]
    run: cp model/report.txt `+filepath.Join(dir, "published.txt")+`
`)

	mustRun(t, path, "--worker", "do=do://launch/?region="+region)

	published := readFileString(t, filepath.Join(dir, "published.txt"))

	for _, want := range []string{"seed", "x86_64", "do"} {
		if !strings.Contains(published, want) {
			t.Errorf("published = %q, want it to contain %q", published, want)
		}
	}
}
