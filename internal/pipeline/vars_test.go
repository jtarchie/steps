package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
)

// TestRenderStepVarsLeavesTheLoadedConfigAlone pins that rendering a step
// produces a rendered COPY.
//
// A long-lived process — steps web, steps watch — loads one *config.Config and
// hands it to every run. Writing rendered text back into it means the second
// run finds no placeholder left and sends the first run's values. Worse, the
// substituted text is what gets hashed (see renderStepVars' own doc), so the
// second run hashes identically to the first: a false cache hit, which is the
// exact thing that doc says this function exists to prevent.
//
// messages: made this reachable. The scalar Run/Image/Dir fields are copied by
// the struct assignment; a slice shares its backing array with the config it
// came from.
func TestRenderStepVarsLeavesTheLoadedConfigAlone(t *testing.T) {
	t.Parallel()

	loaded := config.Step{
		Agent:    "reviewer",
		Messages: []string{"Review ((.:version))."},
	}

	first := renderWith(t, loaded, "version", "1.2.3")
	if got := first.Messages[0]; got != "Review 1.2.3." {
		t.Fatalf("first render = %q, want the captured value substituted", got)
	}

	if got := loaded.Messages[0]; got != "Review ((.:version))." {
		t.Fatalf("the loaded config was rewritten to %q — a second run has no placeholder left", got)
	}

	second := renderWith(t, loaded, "version", "2.0.0")
	if got := second.Messages[0]; got != "Review 2.0.0." {
		t.Errorf("second render = %q, want the second run's own value", got)
	}
}

// TestAnApprovalNamesWhatItWouldPublish: an approval's message is rendered like a command, into a copy.
func TestAnApprovalNamesWhatItWouldPublish(t *testing.T) {
	t.Parallel()

	loaded := config.Step{Approval: &config.Approval{Message: "Publish ((.:tag))?"}}

	if got := renderWith(t, loaded, "tag", "v1.2.3").Approval.Message; got != "Publish v1.2.3?" {
		t.Errorf("the approval asks %q, want the captured tag", got)
	}

	if got := loaded.Approval.Message; got != "Publish ((.:tag))?" {
		t.Errorf("the loaded approval was rewritten to %q", got)
	}
}

// TestAValueTheRunNeverCapturedFailsTheStep: load refuses this, so only a run
// that went somewhere load could not see reaches it — and the literal
// reference must not reach a shell.
func TestAValueTheRunNeverCapturedFailsTheStep(t *testing.T) {
	t.Parallel()

	_, _, err := renderStepVars(withRunVars(context.Background()), config.Step{Run: "echo ((.:nope))"})
	if err == nil || !strings.Contains(err.Error(), "((.:nope)) has no value") {
		t.Errorf("rendering a never-captured value = %v, want it named as missing", err)
	}
}

func renderWith(t *testing.T, step config.Step, name, value string) config.Step {
	t.Helper()

	ctx := withRunVars(context.Background())
	runVarsFrom(ctx).set(name, capture{value: value})

	rendered, _, err := renderStepVars(ctx, step)
	if err != nil {
		t.Fatal(err)
	}

	return rendered
}

// TestACellsCapturesAreItsOwn: a cell reads what the run captured before the
// matrix, and what it captures stays in the cell. No pipeline can observe this
// end to end yet — a matrix's nested steps are not templated per cell, so
// every cell captures the same value.
func TestACellsCapturesAreItsOwn(t *testing.T) {
	t.Parallel()

	run := withRunVars(context.Background())
	runVarsFrom(run).set("before", capture{value: "outer"})

	cell := withCellVars(run)
	runVarsFrom(cell).set("tag", capture{value: "a"})

	rendered, _, err := renderStepVars(cell, config.Step{Run: "((.:before)) ((.:tag))"})
	if err != nil || rendered.Run != "outer a" {
		t.Errorf("the cell rendered (%q, %v), want the run's value and its own", rendered.Run, err)
	}

	if _, leaked := runVarsFrom(run).lookup("tag"); leaked {
		t.Error("a cell's capture reached the run's scope")
	}
}

// TestOnlyAGetsFactsKeepAStepFromReuse: a load_var: value is hashed with the
// step it renders into, so the cache may key on it; a get's facts mark the
// step as one no cache may stand in for.
func TestOnlyAGetsFactsKeepAStepFromReuse(t *testing.T) {
	t.Parallel()

	ctx := withRunVars(context.Background())
	setFacts(ctx, "pr", []events.Fact{{Name: "url", Value: "https://x/1"}})
	runVarsFrom(ctx).set("tag", capture{value: "v1"})

	_, readsFacts, err := renderStepVars(ctx, config.Step{Run: "echo ((.:tag))"})
	if err != nil || readsFacts {
		t.Errorf("a step reading only a load_var: = (%v, %v), want (false, nil)", readsFacts, err)
	}

	rendered, readsFacts, err := renderStepVars(ctx, config.Step{Run: "echo ((.:pr.url))"})
	if err != nil || !readsFacts || rendered.Run != "echo https://x/1" {
		t.Errorf("a step reading a fact = (%q, %v, %v), want it rendered and said", rendered.Run, readsFacts, err)
	}

	if !reusable(ctx) || reusable(withoutReuse(ctx)) {
		t.Error("withoutReuse is what makes a step unreusable")
	}
}

// TestReadVarFileAcceptsExactlyTheLimit pins the cap's boundary: the e2e
// refuses a byte over, which a `>=` would also satisfy.
func TestReadVarFileAcceptsExactlyTheLimit(t *testing.T) {
	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "at"), bytes.Repeat([]byte("a"), maxVarFileBytes), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	body, err := readVarFile(dir, "at")
	if err != nil {
		t.Fatalf("a file of exactly %d bytes was refused: %v", maxVarFileBytes, err)
	}

	if len(body) != maxVarFileBytes {
		t.Errorf("read %d bytes, want %d", len(body), maxVarFileBytes)
	}
}

func TestReadConfinedFileRefusesASymlinkOutOfTheTree(t *testing.T) {
	t.Parallel()

	outside := filepath.Join(t.TempDir(), "secret")

	err := os.WriteFile(outside, []byte("s3cret"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	err = os.Symlink(outside, filepath.Join(dir, "link"))
	if err != nil {
		t.Fatal(err)
	}

	body, err := readConfinedFile(dir, "link", 10)
	if err == nil {
		t.Fatalf("read %q through a symlink out of the tree", body)
	}
}
