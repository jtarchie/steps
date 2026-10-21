package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
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
		Messages: []string{"Review ((version))."},
	}

	first := renderWith(t, loaded, "version", "1.2.3")
	if got := first.Messages[0]; got != "Review 1.2.3." {
		t.Fatalf("first render = %q, want the captured value substituted", got)
	}

	if got := loaded.Messages[0]; got != "Review ((version))." {
		t.Fatalf("the loaded config was rewritten to %q — a second run has no placeholder left", got)
	}

	second := renderWith(t, loaded, "version", "2.0.0")
	if got := second.Messages[0]; got != "Review 2.0.0." {
		t.Errorf("second render = %q, want the second run's own value", got)
	}
}

func renderWith(t *testing.T, step config.Step, name, value string) config.Step {
	t.Helper()

	ctx := withRunVars(context.Background())
	runVarsFrom(ctx).set(name, value)

	return renderStepVars(ctx, step)
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
