package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// The seam from the minted run id and recorded revision to the environment a step sees. The second run being a cache hit is the "never hashed" claim: the merkle key did not move though the run id did.
func TestBuildMetadataNamesTheRecordedRun(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "meta.db")
	log := filepath.Join(dir, "meta.log")

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: stamp
    inputs: []
    run: echo "$STEPS_RUN_ID $STEPS_PIPELINE_REVISION $STEPS_PIPELINE_NAME" >> `+log+`
`)

	for range 2 {
		err := cli.Run([]string{"run", path, "--job", "build", "--db", state})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(readFileString(t, log)), "\n")
	if len(lines) != 1 {
		t.Fatalf("task ran %d times, want 1: the run id must not enter the cache key", len(lines))
	}

	name := cli.PipelineName(path)
	runs := listRuns(t, state, name, "build")
	oldest := runs[len(runs)-1]

	if want := oldest.ID + " " + oldest.ConfigSHA + " " + name; lines[0] != want {
		t.Errorf("step saw %q, want %q", lines[0], want)
	}
}

func TestBuildMetadataUnderTheDaemon(t *testing.T) {
	for _, tc := range []struct{ name, flag, want string }{
		{name: "default", want: ""},
		{name: "external-url", flag: "https://ci.example/steps/", want: "https://ci.example/steps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "seen")

			path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: stamp
    inputs: []
    run: echo "$STEPS_URL $STEPS_PIPELINE_NAME $STEPS_PIPELINE_REVISION" > `+out+`
`)

			args := []string{"--interval", "1h"}
			if tc.flag != "" {
				args = append(args, "--external-url", tc.flag)
			}

			served := startWebFor(t, path, args...)
			defer served.stopIfRunning(t)

			name := cli.PipelineName(path)

			served.trigger(t, name, "build")
			waitForFile(t, out)

			run := newestRun(t, served.state, name, "build")

			want := tc.want
			if want == "" {
				want = "http://" + served.addr
			}

			if got := readTrimmed(t, out); got != want+" "+name+" "+run.ConfigSHA {
				t.Errorf("step saw %q, want %q", got, want+" "+name+" "+run.ConfigSHA)
			}

			served.stop(t)
		})
	}
}

// Crosses the orchestrator-to-shim seam through the real transport.
func TestBuildMetadataReachesAPlacedStep(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "seen")

	path := writePipeline(t, dir, `
jobs:
- name: placed
  plan:
  - task: stamp
    tags: [gpu]
    inputs: []
    run: echo "$STEPS_RUN_ID ${STEPS_WORKER-none}" > `+out+`
`)

	err := cli.Run([]string{"run", path, "--job", "placed", "--db", filepath.Join(dir, "p.db"), "--worker", "gpu=local:"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := strings.Fields(readFileString(t, out))
	if len(got) != 2 || len(got[0]) != 16 || got[1] == "none" {
		t.Errorf("placed step saw %q, want a run id and STEPS_WORKER", got)
	}
}

// A check is not part of a build: a run id in a version would mint one per run.
func TestBuildMetadataNeverReachesACheck(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "c.db")
	out := filepath.Join(dir, "seen")

	path := writePipeline(t, dir, `
resource_types:
- name: probe
  config:
    check: echo '[{"ref":"'"${STEPS_RUN_ID-none}"'"}]'
    in: echo {{ .version.ref | shellquote }} >> `+out+`
resources:
- name: r
  type: probe
  source: {}
jobs:
- name: build
  plan:
  - get: r
`)

	for range 2 {
		err := cli.Run([]string{"run", path, "--job", "build", "--db", state, "--force"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if got := strings.TrimSpace(readFileString(t, out)); got != "none\nnone" {
		t.Errorf("the check's version was %q across two runs, want a stable none", got)
	}
}
