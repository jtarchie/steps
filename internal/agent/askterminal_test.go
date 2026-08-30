package agent

import "testing"

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
