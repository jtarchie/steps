package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// TestRunDrawsALiveViewWhenAskedFor is #124 end to end: --progress=tty replaces the plain lines with a live region, leaves one line per finished step in scrollback, ends on a summary, and — because the job failed — prints the failed step's output after it, the way buildx does. Not parallel: captureStdout swaps os.Stdout.
func TestRunDrawsALiveViewWhenAskedFor(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: compile
    run: "echo compiled"
  - try:
      task: flaky
      run: "exit 3"
  - task: lint
    run: "echo 'lint: 2 problems' && exit 1"
`)

	var runErr error

	out := captureStdout(t, func() { runErr = cli.Run([]string{"run", path, "--job", "build", "--progress", "tty"}) })
	if runErr == nil {
		t.Fatal("the job should have failed on lint")
	}

	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("nothing was drawn — the live view never ran:\n%q", out)
	}

	screen := ansi.ReplaceAllString(out, "")

	for _, want := range []string{
		"✓ task compile",
		"try: flaky failed (tried, continuing)",
		"✗ task lint failed",
		"✗ build failed",
		"lint: 2 problems",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("scrollback is missing %q:\n%s", want, screen)
		}
	}

	if strings.Contains(screen, "task: compile\n") {
		t.Errorf("the plain renderer printed too, so the terminal got both:\n%s", screen)
	}

	summary := strings.Index(screen, "✗ build failed")
	if tail := strings.LastIndex(screen, "lint: 2 problems"); summary < 0 || tail < summary {
		t.Errorf("the failed step's output should follow the summary:\n%s", screen)
	}
}

const followPipeline = `
jobs:
- name: build
  plan:
  - task: compile
    run: "echo compiled"
  - task: lint
    run: "echo 'lint: 2 problems' && exit 1"
`

// lastRunID is the run a `steps run` of path just recorded.
func lastRunID(t *testing.T, path string) string {
	t.Helper()

	st, err := sqlite.OpenExisting(string(cli.StatePath(path, "")), cli.PipelineName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), "", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v", runs, err)
	}

	return runs[0].ID
}

// TestRunsFollowReplaysARunInTheLiveView is the rest of #124: the same view, attached to a run by reading what it recorded, exiting as the run did.
func TestRunsFollowReplaysARunInTheLiveView(t *testing.T) {
	path := writePipeline(t, t.TempDir(), followPipeline)

	_ = captureStdout(t, func() { _ = cli.Run([]string{"run", path, "--job", "build", "--progress", "plain"}) })
	id := lastRunID(t, path)

	var followErr error

	out := captureStdout(t, func() {
		followErr = cli.Run(append([]string{"runs", "follow", id, "--progress", "tty"}, readArgs(path)...))
	})

	if code := outcome.ExitCode(followErr); code != outcome.ExitFailed {
		t.Errorf("exit %d (%v), want the failed run's %d", code, followErr, outcome.ExitFailed)
	}

	screen := ansi.ReplaceAllString(out, "")

	for _, want := range []string{
		"following " + id,
		"✓ task compile",
		"✗ task lint failed",
		"✗ build failed",
		"lint: 2 problems",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("scrollback is missing %q:\n%s", want, screen)
		}
	}

	summary := strings.Index(screen, "✗ build failed")
	if tail := strings.LastIndex(screen, "lint: 2 problems"); summary < 0 || tail < summary {
		t.Errorf("the failed step's output should follow the summary:\n%s", screen)
	}
}

// TestRunsFollowPlainPrintsWhatAStreamingRunWould: plain never prints step_output locally because the bytes streamed; a follower has nothing else.
func TestRunsFollowPlainPrintsWhatAStreamingRunWould(t *testing.T) {
	path := writePipeline(t, t.TempDir(), followPipeline)

	_ = captureStdout(t, func() { _ = cli.Run([]string{"run", path, "--job", "build", "--progress", "plain"}) })
	id := lastRunID(t, path)

	var followErr error

	out := captureStdout(t, func() {
		followErr = cli.Run(append([]string{"runs", "follow", "--progress", "plain"}, readArgs(path)...))
	})
	if outcome.ExitCode(followErr) != outcome.ExitFailed {
		t.Errorf("err = %v, want the run's failure", followErr)
	}

	for _, want := range []string{"following " + id, "task: compile", "lint: 2 problems", "build failed in"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain drew escapes:\n%q", out)
	}
}

func TestRunsFollowRefusals(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, followPipeline)

	var err error

	_ = captureStdout(t, func() { err = cli.Run(append([]string{"runs", "follow"}, readArgs(path)...)) })
	if outcome.ExitCode(err) != outcome.ExitErrored || !strings.Contains(err.Error(), "no runs recorded yet") {
		t.Errorf("before any run: err = %v, want nothing to follow refused", err)
	}

	_, statErr := os.Stat(filepath.Join(dir, ".steps"))
	if !os.IsNotExist(statErr) {
		t.Errorf("following created state: %v", statErr)
	}

	_ = captureStdout(t, func() { _ = cli.Run([]string{"run", path, "--job", "build", "--progress", "plain"}) })

	_ = captureStdout(t, func() { err = cli.Run(append([]string{"runs", "follow", "nope"}, readArgs(path)...)) })
	if err == nil || !strings.Contains(err.Error(), cli.PipelineName(path)) {
		t.Errorf("unknown run: err = %v, want the pipeline named", err)
	}

	_ = captureStdout(t, func() { err = cli.Run(append([]string{"runs", "follow", "--job", "deploi"}, readArgs(path)...)) })
	if outcome.ExitCode(err) != outcome.ExitErrored || !strings.Contains(err.Error(), `"deploi"`) {
		t.Errorf("a job with no runs: err = %v, want refused naming the job", err)
	}

	err = cli.Run(append([]string{"runs", "follow", "nope", "--job", "build"}, readArgs(path)...))
	if err == nil || !strings.Contains(err.Error(), "--job") {
		t.Errorf("run with --job: err = %v, want refused", err)
	}
}
