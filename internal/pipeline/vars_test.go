package pipeline

import (
	"context"
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
