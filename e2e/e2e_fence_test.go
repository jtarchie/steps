package e2e

// fence: is a daemon-config fact judging a pipeline the daemon was sent, so every test here crosses the seam: a config file, a set over HTTP, the answer printed by the terminal that asked, and the page.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/web"
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

// A word the daemon does not know is refused rather than read as off: an operator who mistyped enforce believes the daemon refuses what it would run.
func TestFenceRefusesAnUnknownPolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := stepsRun(t, []string{"web", "--listen", "127.0.0.1:1", "--db", filepath.Join(dir, "daemon.db"), "--fence", "loud"})
	if err == nil || !strings.Contains(err.Error(), "warn") || !strings.Contains(err.Error(), "enforce") {
		t.Fatalf("web --fence loud = %v, want a refusal naming off, warn and enforce", err)
	}

	config := filepath.Join(dir, "steps.yml")
	writeConfigFile(t, config, "web:\n  fence: enforce\n  allow-unfenced:\n    app: task loose\n")

	err = stepsRun(t, []string{"web", "--listen", "127.0.0.1:1", "--db", filepath.Join(dir, "daemon.db"), "--config", config})
	if err == nil || !strings.Contains(err.Error(), "allow-unfenced") {
		t.Fatalf("web with allow-unfenced: {app: <a word>} = %v, want a refusal naming allow-unfenced", err)
	}
}

// validate reads the daemon's file, and holds its fence: to the same words the daemon does.
func TestValidateRefusesAFenceTheDaemonWould(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	path := filepath.Join(dir, "app.yml")
	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: loose\n    run: \"true\"\n")

	for _, tc := range []struct{ fence, want string }{
		{"loud", "not a policy"},
		{"[warn]", "web.fence"},
		{"enforce\n  allow-unfenced: [task loose]", "allow-unfenced"},
		{"enforce\n  allow-unfenced: {app: [tasks loose]}", "not an entity"},
	} {
		writeConfigFile(t, config, "web:\n  fence: "+tc.fence+"\n")

		err := stepsRun(t, []string{"validate", "--syntax-only", "--config", config, path})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("validate --config <fence: %s> = %v, want a refusal naming %q", tc.fence, err, tc.want)
		}
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

// The two keys of enforce's opt-out: the pipeline's unfenced: reason on the entity, and the daemon file's allow-unfenced: naming that pipeline and entity.
const (
	enforceConfig = "web:\n  fence: enforce\n  allow-unfenced:\n    app: [task loose]\n"
	loosePipeline = "jobs:\n- name: build\n  plan:\n  - task: loose\n    run: \"true\"\n"
	optedOut      = "jobs:\n- name: build\n  plan:\n  - task: loose\n    unfenced: posts with the host's own credentials\n    run: \"true\"\n"
)

// enforce refuses an unfenced command at set unless BOTH keys name it; a pipeline cannot loosen the policy that judges it.
func TestFenceEnforceRefusesUnlessBothKeysOptOut(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, enforceConfig)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--config", config)
	defer served.stop(t)

	for _, tc := range []struct{ why, name, body string }{
		{"the config's key alone is the operator's, and says nothing about why", "app", loosePipeline},
		{"the pipeline's key alone is self-certification", "other", optedOut},
		{"a renamed entity drops off the allowlist", "app", strings.ReplaceAll(optedOut, "loose", "renamed")},
	} {
		writePipelineFile(t, path, tc.body)

		err := stepsRun(t, []string{"pipeline", "set", "-p", tc.name, "-c", path, "-n", "--target", served.target()})
		if err == nil || !strings.Contains(err.Error(), "fence: enforce") || !strings.Contains(err.Error(), "set image:") {
			t.Fatalf("%s, yet enforce accepted it: %v", tc.why, err)
		}

		t.Logf("%s: %v", tc.why, err)
	}

	writePipelineFile(t, path, optedOut)

	out := captureStdout(t, func() { served.set(t, "app", path) })
	if !strings.Contains(out, `task "loose"`) || !strings.Contains(out, "posts with the host's own credentials") {
		t.Errorf("set's answer does not list the opted-out task with its reason:\n%s", out)
	}

	if _, page := served.get(t, "/p/app"); !strings.Contains(page, "posts with the host&#39;s own credentials") {
		t.Errorf("the pipeline page does not show the opted-out task's reason:\n%s", page)
	}
}

// A refusal names what is missing: the pipeline's unfenced:, or the daemon's allow-unfenced: entry by its entity.
func TestFenceEnforceNamesTheMissingKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, enforceConfig)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--config", config)
	defer served.stop(t)

	for _, tc := range []struct{ name, body, want string }{
		{"app", loosePipeline, "no unfenced:"},
		{"other", optedOut, `allow-unfenced: does not name "task loose" for other`},
		{"app", strings.ReplaceAll(optedOut, "loose", "renamed"), `allow-unfenced: does not name "task renamed" for app`},
	} {
		writePipelineFile(t, path, tc.body)

		err := stepsRun(t, []string{"pipeline", "set", "-p", tc.name, "-c", path, "-n", "--target", served.target()})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("set -p %s = %v, want a refusal saying %q", tc.name, err, tc.want)
		}
	}
}

// At the boot restore too: a stored pipeline the daemon's policy no longer accepts is shown broken, and served again once it does.
func TestFenceEnforceJudgesTheRestore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	state := filepath.Join(dir, "daemon.db")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, enforceConfig)
	writePipelineFile(t, path, optedOut)

	served := startWeb(t, "--db", state, "--config", config)
	served.set(t, "app", path)
	served.stop(t)

	// The operator withdraws the allowlist: the stored pipeline no longer complies, so it is shown broken rather than dropped or served.
	writeConfigFile(t, config, "web:\n  fence: enforce\n")

	restarted := startWeb(t, "--db", state, "--config", config)

	// The daemon's policy, not the file, is what changed, so the hint says so rather than asking for a set.
	if reason := restarted.brokenReason(t, "app"); !strings.Contains(reason, `task "loose"`) || !strings.Contains(reason, "allow-unfenced") || !strings.Contains(reason, "restart the daemon") {
		t.Errorf("a stored pipeline that enforce no longer accepts is not shown broken with why: %q", reason)
	}

	restarted.stop(t)

	writeConfigFile(t, config, enforceConfig)

	again := startWeb(t, "--db", state, "--config", config)
	defer again.stop(t)

	if reason := again.brokenReason(t, "app"); reason != "" {
		t.Errorf("a stored pipeline both keys opt out is broken after a restart: %s", reason)
	}
}

// A command the sandbox fences on this machine needs no opt-out under enforce.
func TestFenceEnforceAcceptsWhatIsFenced(t *testing.T) {
	requireSandbox(t)
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, "web:\n  fence: enforce\n")
	writePipelineFile(t, path, "jobs:\n- name: build\n  plan:\n  - task: fenced\n    sandbox: true\n    run: \"true\"\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--config", config)
	defer served.stop(t)

	out := captureStdout(t, func() { served.set(t, "app", path) })
	if strings.Contains(out, "unfenced") {
		t.Errorf("enforce listed a sandboxed task as unfenced:\n%s", out)
	}
}

// Under warn the reason is shown beside the listed command, and changes nothing else.
func TestFenceWarnShowsTheReason(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, "web:\n  fence: warn\n")
	writePipelineFile(t, path, optedOut)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--config", config)
	defer served.stop(t)

	out := captureStdout(t, func() { served.set(t, "app", path) })
	if !strings.Contains(out, `task "loose"`) || !strings.Contains(out, "posts with the host's own credentials") || !strings.Contains(out, "task loose") {
		t.Errorf("warn does not show the opt-out's entity and reason beside the listed task:\n%s", out)
	}
}

// validate --config holds the file to enforce as a set would, by the name it would be set as.
func TestValidateReportsAgainstEnforce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, enforceConfig)
	writePipelineFile(t, path, loosePipeline)

	err := stepsRun(t, []string{"validate", "--syntax-only", "--config", config, path})
	if err == nil || !strings.Contains(err.Error(), `task "loose"`) || !strings.Contains(err.Error(), "no unfenced:") {
		t.Fatalf("validate --config under enforce passed an unfenced task: %v", err)
	}

	writePipelineFile(t, path, optedOut)

	err = stepsRun(t, []string{"validate", "--syntax-only", "--config", config, path})
	if err != nil {
		t.Fatalf("validate --config under enforce refused what both keys opt out: %v", err)
	}

	err = stepsRun(t, []string{"validate", "--syntax-only", "--config", config, "--name", "other=" + path, path})
	if err == nil || !strings.Contains(err.Error(), "allow-unfenced") {
		t.Fatalf("validate --config --name other= under enforce passed what only app is allowed: %v", err)
	}
}

// brokenReason is why /api/pipelines lists name as held and not served, empty when it does not.
func (w *webProcess) brokenReason(t *testing.T, name string) string {
	t.Helper()

	_, body := w.get(t, "/api/pipelines")

	var rows []web.PipelineSummary

	err := json.Unmarshal([]byte(body), &rows)
	if err != nil {
		t.Fatalf("/api/pipelines: %v: %s", err, body)
	}

	for _, row := range rows {
		if row.Name == name {
			return row.Broken
		}
	}

	return ""
}
