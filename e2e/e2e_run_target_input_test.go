package e2e

// steps#283: `steps run --target --input GET=PATH` uploads a folder from this machine and the daemon's one-off uses it in place of that get, as Concourse's `fly execute -i` does.

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store/sqlite"
)

// unreachablePipeline gets a resource whose check and in: both fail and leave a mark, so a run that used the upload proves it never asked the resource.
func unreachablePipeline(t *testing.T, dir string) string {
	t.Helper()

	asked := filepath.Join(dir, "asked")

	return writePipeline(t, dir, `
resource_types:
- name: unreachable
  config:
    check: echo check >> `+asked+` && exit 1
    in: echo in >> `+asked+` && exit 1

resources:
- name: repo
  type: unreachable
  source: {}

jobs:
- name: build
  plan:
  - get: src
    resource: repo
    version: every
  - task: show
    inputs: [src]
    run: |
      cat src/hello.txt src/nested/deep.txt
      for f in ignored.txt secret.txt; do test -e src/$f && echo "sent $f"; done
      echo listed-all
`)
}

// checkout is a git repository with a committed file, an untracked one, an ignored one and one a user excludes.
func checkout(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for name, body := range map[string]string{
		"hello.txt":       "hello-from-the-laptop\n",
		"nested/deep.txt": "untracked-but-not-ignored\n",
		".gitignore":      "ignored.txt\n",
		"ignored.txt":     "git-ignored\n",
		"secret.txt":      "excluded-by-flag\n",
	} {
		path := filepath.Join(dir, name)

		err := os.MkdirAll(filepath.Dir(path), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(path, []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"init", "-q"}, {"add", "hello.txt", ".gitignore"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // constant arguments
		cmd.Dir = dir

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	return dir
}

func TestRunTargetInputReplacesTheGetWithTheUploadedFolder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := unreachablePipeline(t, dir)
	state := filepath.Join(dir, "daemon.db")

	served := startWeb(t, "--db", state, "--interval", "50ms")
	defer served.stopIfRunning(t)

	out := runWithInput(t, path, served, "--exclude", "secret.txt")

	for _, want := range []string{"hello-from-the-laptop", "untracked-but-not-ignored", "listed-all"} {
		if !strings.Contains(out, want) {
			t.Errorf("the task never printed %q from the uploaded folder:\n%s", want, out)
		}
	}

	for _, sent := range []string{"sent ignored.txt", "sent secret.txt"} {
		if strings.Contains(out, sent) {
			t.Errorf("%s: a git-ignored or --exclude'd file was uploaded:\n%s", sent, out)
		}
	}

	if fileExists(filepath.Join(dir, "asked")) {
		t.Error("the overridden get's resource was checked or fetched")
	}

	assertNoVersionRecorded(t, state, strings.Split(regexp.MustCompile(`/p/[^/\s]+/runs/`).FindString(out), "/")[2])

	// --include-ignored sends what git would leave out, as fly's does.
	out = runWithInput(t, path, served, "--include-ignored")
	if !strings.Contains(out, "sent ignored.txt") || !strings.Contains(out, "sent secret.txt") {
		t.Errorf("--include-ignored left files out:\n%s", out)
	}
}

func runWithInput(t *testing.T, path string, served *webProcess, extra ...string) string {
	t.Helper()

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, append([]string{"run", path, "--job", "build", "--target", served.target(), "--progress", "plain",
			"--input", "src=" + checkout(t)}, extra...))
	})
	if err != nil {
		t.Fatalf("steps run --target --input %v: %v\n%s", extra, err, out)
	}

	return out
}

// assertNoVersionRecorded holds the upload to being a one-off version of the get, never one of the resource's: nothing a later run could take as its newest or as having passed.
func assertNoVersionRecorded(t *testing.T, state, pipelineName string) {
	t.Helper()

	st, err := sqlite.OpenStore(state, pipelineName)
	if err != nil {
		t.Fatalf("open the one-off's state: %v", err)
	}

	defer func() { _ = st.Close() }()

	versions, err := st.ResourceVersionsJSON(t.Context(), "repo")
	if err != nil || len(versions) != 0 {
		t.Errorf("the resource's version history = %v, %v; want the upload kept out of it", versions, err)
	}

	_, found, _ := st.LastChecked(t.Context(), "repo")
	if found {
		t.Error("the upload was recorded as the resource's checked version")
	}

	passed, err := st.PassedVersions(t.Context(), "build", 10)
	if err != nil || len(passed) != 0 {
		t.Errorf("passed versions = %v, %v; want the upload never recorded as a version that passed build", passed, err)
	}

	mark, err := st.ConsumedMark(t.Context(), "build", "repo")
	if err != nil || mark != 0 {
		t.Errorf("the version: every cursor = %d, %v; want the upload never taken as one of the resource's versions", mark, err)
	}
}

// A refused upload starts nothing: no one-off pipeline is served, and the resource is never asked.
func TestRunTargetInputRefusals(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := unreachablePipeline(t, dir)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--one-off-input-max-bytes", "16")
	defer served.stopIfRunning(t)

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		// Refused before anything is sent: this target answers nothing.
		"a get the job lacks": {[]string{"--target", "http://127.0.0.1:1", "--input", "nope=" + checkout(t)}, `"nope"`},
		"past the size cap":   {[]string{"--target", served.target(), "--input", "src=" + checkout(t)}, "more than 16 bytes, this daemon's --one-off-input-max-bytes"},
		"without a target":    {[]string{"--input", "src=" + checkout(t)}, "--input"},
	} {
		t.Run(name, func(t *testing.T) {
			err := stepsRun(t, append([]string{"run", path, "--job", "build", "--progress", "plain"}, tc.args...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("steps run %v = %v, want refused naming %s", tc.args, err, tc.want)
			}
		})
	}

	if code, body := served.get(t, "/api/pipelines"); code != http.StatusOK || strings.Contains(body, "one-off.") {
		t.Errorf("GET /api/pipelines = %d %s, want no one-off served after only refused uploads", code, body)
	}

	if fileExists(filepath.Join(dir, "asked")) {
		t.Error("a refused upload still reached the resource")
	}
}
