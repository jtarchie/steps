package e2e

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
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
