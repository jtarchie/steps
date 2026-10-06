package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// The views a person reads after a run, each asserted on the sentence it exists to print; not parallel, they capture stdout.
func TestTheReadViewsSayWhatTheyFound(t *testing.T) {
	path := flagFixture(t)
	state := string(StatePath(path, ""))
	read := []string{"-p", PipelineName(path), "--db", state}

	var err error

	out := captureStdout(t, func() { err = Run([]string{"plan", path}) })
	if err != nil || !strings.Contains(out, "1 step(s): 1 would run, 0 cached") {
		t.Errorf("plan = %v, printing:\n%s\nwant the tally", err, out)
	}

	_ = captureStdout(t, func() { err = Run([]string{"run", path}) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	out = captureStdout(t, func() { err = Run(append([]string{"runs", "follow", "--progress", "plain"}, read...)) })
	if err != nil || !strings.Contains(out, "already succeeded, replaying") {
		t.Errorf("follow of a finished run = %v, printing:\n%s\nwant it called a replay", err, out)
	}

	out = captureStdout(t, func() { err = Run([]string{"runs", "--db", state}) })
	if err != nil || !strings.Contains(out, "\n\nWHEN") || !strings.Contains(out, "break one down with: steps runs cost") {
		t.Errorf("runs across the file = %v, printing:\n%s\nwant the pipelines, a blank line, the runs, and the hint", err, out)
	}
}

// Before anything is recorded the view still names the pipeline asked about, and a run id with --job is refused rather than one of them ignored; not parallel, it captures stdout.
func TestRunsStepsWithNothingToRead(t *testing.T) {
	read := []string{"-p", "app", "--db", filepath.Join(t.TempDir(), "none.db")}

	var err error

	out := captureStdout(t, func() { err = Run(append([]string{"runs", "steps"}, read...)) })
	if err != nil || !strings.Contains(out, "no runs recorded yet for app") {
		t.Errorf("steps of a pipeline with no state = %v, printing %q; want it named", err, out)
	}

	err = Run(append([]string{"runs", "steps", "RUN", "--job", "build"}, read...))
	if err == nil || !strings.Contains(err.Error(), "--job does not apply to a named run") {
		t.Errorf("runs steps with a run and --job = %v, want refused", err)
	}
}

func TestFirstLineFitsATableCell(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 100)

	for text, want := range map[string]string{
		"":                      "-",
		"short":                 "short",
		"first\nsecond":         "first",
		long:                    strings.Repeat("x", 69) + "…",
		strings.Repeat("y", 70): strings.Repeat("y", 70),
	} {
		if got := firstLine(text); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", text, got, want)
		}
	}
}

// 0 of 0 is not 0%: a step that reported no usage shows a dash, and is never a division by zero.
func TestCachePercentOfNothingIsADash(t *testing.T) {
	t.Parallel()

	if got := cachePercent(0, 0); got != "-" {
		t.Errorf("cachePercent(0, 0) = %q, want -", got)
	}

	if got := cachePercent(200, 50); got != "25" {
		t.Errorf("cachePercent(200, 50) = %q, want 25", got)
	}
}
