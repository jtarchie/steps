package agent

// What a fix: agent is actually asked.

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestFixMessagesAreSeparateTurns pins that a fix: gets the same messages:
// semantics a step does.
//
// They were joined with a blank line into ONE turn, which is exactly the
// distinction a list of messages exists to draw: "do this, and once you are
// done, do that" collapses into "do both", and the second instruction — which
// is usually the check on the first — is answered in the same breath as the
// work it was meant to verify.
func TestFixMessagesAreSeparateTurns(t *testing.T) {
	t.Parallel()

	fix := &config.FixSpec{Messages: []string{"Repair the build.", "Now say what you changed."}}

	messages := buildFixMessages(fix, config.ResolvedTask{Name: "build"}, "exit status 1", t.TempDir())

	if len(messages) != 2 {
		t.Fatalf("got %d turn(s), want one per message: %q", len(messages), messages)
	}

	if !strings.HasPrefix(messages[0], "Repair the build.") {
		t.Errorf("first turn = %q, want the first message", messages[0])
	}

	// The failure is what the whole conversation is about, so it opens the
	// work rather than arriving after it.
	if !strings.Contains(messages[0], "exit status 1") {
		t.Error("the failure output did not reach the opening turn")
	}

	if messages[1] != "Now say what you changed." {
		t.Errorf("second turn = %q, want the second message alone", messages[1])
	}
}

// TestFixMessagesLeaveTheConfigAlone pins that assembling them does not write
// the failure output back into the loaded pipeline.
//
// A fix: runs once per failed attempt, and steps web holds one *config.Config
// across every run — so appending in place would give the second attempt the
// first attempt's failure, and every attempt after that all of them.
func TestFixMessagesLeaveTheConfigAlone(t *testing.T) {
	t.Parallel()

	fix := &config.FixSpec{Messages: []string{"Repair the build."}}

	buildFixMessages(fix, config.ResolvedTask{Name: "build"}, "first failure", t.TempDir())

	if fix.Messages[0] != "Repair the build." {
		t.Fatalf("the loaded fix: now reads %q — the next attempt inherits the last one's failure", fix.Messages[0])
	}
}

// TestFixWithNoMessagesIsOneTurn pins that the default path did not move.
func TestFixWithNoMessagesIsOneTurn(t *testing.T) {
	t.Parallel()

	messages := buildFixMessages(&config.FixSpec{}, config.ResolvedTask{Name: "build"}, "boom", t.TempDir())

	if len(messages) != 1 {
		t.Fatalf("got %d turn(s), want 1: %q", len(messages), messages)
	}

	if !strings.Contains(messages[0], "build") || !strings.Contains(messages[0], "boom") {
		t.Errorf("turn = %q, want the default prompt naming the task and carrying the failure", messages[0])
	}
}
