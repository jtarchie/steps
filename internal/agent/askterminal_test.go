package agent

import (
	"context"
	"testing"
	"time"
)

// TestTerminalPrompterIsAbsentWithoutATerminal is the fence that decides
// whether a question is put to somebody inline or parked. Under `go test`
// stdin is not a character device, which is the same shape as every CI run —
// so the prompter must be nil and the ladder must fall through to the parked
// rung rather than blocking on a read nobody will answer.
func TestTerminalPrompterIsAbsentWithoutATerminal(t *testing.T) {
	if stdinIsTerminal() {
		t.Skip("this test process has a terminal on stdin")
	}

	if terminalPrompter() != nil {
		t.Error("a prompter was offered with no terminal to prompt at; a parked question would block on a read")
	}
}

// TestTerminalAnswererRecordsWhoIsAtTheKeyboard: the audit record's "who",
// resolved the same way `steps approve` resolves it. Not an authorization
// check — it records who ran the command on this host.
func TestTerminalAnswererRecordsWhoIsAtTheKeyboard(t *testing.T) {
	t.Setenv("STEPS_APPROVER", "jtarchie")

	if got := terminalAnswerer(); got != "jtarchie" {
		t.Errorf("terminalAnswerer = %q, want the configured approver", got)
	}

	t.Setenv("STEPS_APPROVER", "")
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "")

	if got := terminalAnswerer(); got != "terminal" {
		t.Errorf("terminalAnswerer with nothing set = %q, want a stated fallback", got)
	}
}

// TestLockTerminalGivesUpWhenItsQuestionIs: the terminal is a process-wide
// resource, so prompts queue for it — but a prompt whose question was answered
// somewhere else must not have to wait for the one ahead of it to finish
// before it can return. Holding this lock across an uncancellable read was the
// bug: one abandoned prompt disabled the terminal channel for the rest of the
// process, so every later question silently parked with somebody sitting right
// there.
func TestLockTerminalGivesUpWhenItsQuestionIs(t *testing.T) {
	terminalReader.prompt.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() { done <- lockTerminal(ctx) }()

	select {
	case acquired := <-done:
		if acquired {
			t.Error("lockTerminal claimed a lock somebody else was holding")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lockTerminal blocked on a lock its question no longer needs")
	}

	// And it hands the lock straight back once the holder releases it, rather
	// than leaving the terminal claimed by a prompt that gave up.
	terminalReader.prompt.Unlock()

	released := make(chan struct{})

	go func() {
		terminalReader.prompt.Lock()
		close(released)

		terminalReader.prompt.Unlock()
	}()

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal stayed claimed by a prompt that gave up")
	}
}
