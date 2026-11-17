package e2e

// fence: is a daemon-config fact judging a pipeline the daemon was sent, so every test here crosses the seam: a config file, a set over HTTP, the answer printed by the terminal that asked, and the page.

import (
	"path/filepath"
	"strings"
	"testing"
)

// One fenced and one unfenced command: set lists only the unfenced one, and so does the pipeline's page, before and after a restart (the boot restore is the other way a pipeline is accepted).
const fencePipeline = `
jobs:
- name: build
  plan:
  - task: loose
    run: "true"
  - task: fenced
    sandbox: true
    run: "true"
`

func TestFenceWarnListsUnfencedCommandsOnSetAndPage(t *testing.T) {
	requireSandbox(t) // sandbox: true counts as a fence only where it exists

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	state := filepath.Join(dir, "daemon.db")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, "web:\n  fence: warn\n")
	writePipelineFile(t, path, fencePipeline)

	served := startWeb(t, "--db", state, "--config", config)

	out := captureStdout(t, func() { served.set(t, "app", path) })
	if !strings.Contains(out, `task "loose"`) {
		t.Errorf("set's answer does not list the unfenced task:\n%s", out)
	}

	if strings.Contains(out, `task "fenced"`) {
		t.Errorf("set's answer lists a sandboxed task as unfenced:\n%s", out)
	}

	assertPageLists := func(when string, served *webProcess) {
		t.Helper()

		_, page := served.get(t, "/p/app")
		if !strings.Contains(page, "unfenced") || !strings.Contains(page, `task &#34;loose&#34;`) {
			t.Errorf("%s: the pipeline page does not list the unfenced task:\n%s", when, page)
		}

		if strings.Contains(page, `task &#34;fenced&#34;`) {
			t.Errorf("%s: the pipeline page lists a sandboxed task as unfenced", when)
		}
	}

	assertPageLists("after set", served)

	// A set over a served pipeline replaces its list rather than keeping the old one.
	writePipelineFile(t, path, strings.Replace(fencePipeline, "  - task: loose\n", "  - task: renamed\n", 1))
	served.set(t, "app", path)

	if _, page := served.get(t, "/p/app"); !strings.Contains(page, `task &#34;renamed&#34;`) || strings.Contains(page, `task &#34;loose&#34;`) {
		t.Errorf("after a second set the page does not list what that set made unfenced:\n%s", page)
	}

	writePipelineFile(t, path, fencePipeline)
	served.set(t, "app", path)
	served.stop(t)

	restarted := startWeb(t, "--db", state, "--config", config)
	defer restarted.stop(t)

	assertPageLists("after restart", restarted)
}

// off is today's behavior: nothing is listed, so an operator who never asked sees no change.
func TestFenceOffListsNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "app.yml")
	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: loose\n    run: \"true\"\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"))
	defer served.stop(t)

	out := captureStdout(t, func() { served.set(t, "app", path) })
	if strings.Contains(out, "unfenced") {
		t.Errorf("set under the default fence: off listed unfenced commands:\n%s", out)
	}

	if _, page := served.get(t, "/p/app"); strings.Contains(page, `task &#34;loose&#34;`) {
		t.Error("the page under fence: off lists unfenced commands")
	}
}

// enforce is the next step (#263) and needs the opt-out first; a daemon that accepted the word would be one somebody believes is enforcing.
func TestFenceRefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	writeConfigFile(t, config, "web:\n  fence: enforce\n")

	err := stepsRun(t, []string{"web", "--listen", "127.0.0.1:1", "--db", filepath.Join(dir, "daemon.db"), "--config", config})
	if err == nil || !strings.Contains(err.Error(), "enforce") || !strings.Contains(err.Error(), "not available yet") {
		t.Fatalf("web with fence: enforce = %v, want a refusal saying enforce is not available yet", err)
	}

	err = stepsRun(t, []string{"web", "--listen", "127.0.0.1:1", "--db", filepath.Join(dir, "daemon.db"), "--fence", "loud"})
	if err == nil || !strings.Contains(err.Error(), "off") || !strings.Contains(err.Error(), "warn") {
		t.Fatalf("web --fence loud = %v, want a refusal naming off and warn", err)
	}
}

// validate --config judges the file by the policy the daemon would, so a pipeline can be checked before it is set; without the file there is no policy and nothing to report.
func TestValidateReportsAgainstTheDaemonsFence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, "web:\n  fence: warn\n")
	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: loose\n    run: \"true\"\n")

	stderr := captureStderr(t)

	err := stepsRun(t, []string{"validate", "--syntax-only", "--config", config, path})
	if err != nil {
		t.Fatalf("validate --config under fence: warn refused a pipeline it should only report on: %v", err)
	}

	if got := stderr(); !strings.Contains(got, `task "loose"`) || !strings.Contains(got, "unfenced") {
		t.Errorf("validate --config did not report the unfenced task:\n%s", got)
	}
}
