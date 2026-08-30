package agent

// The terminal channel: putting a question to whoever is sitting at the
// terminal that started the run.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/store"
)

// terminalPrompter returns the prompter for this process, or nil when nothing
// is at the other end of stdin — which is every CI run, every `steps watch`
// under a supervisor, and every test. A nil prompter is how the ladder skips
// straight from the responder to a parked question.
func terminalPrompter() askPrompter {
	if !stdinIsTerminal() {
		return nil
	}

	return promptOnTerminal
}

// stdinIsTerminal reports whether stdin is a character device — a terminal
// somebody could type into, rather than a pipe, a file, or /dev/null.
//
// Deliberately stat rather than a terminal library: this decides which of two
// channels to offer, and being wrong in the safe direction (park it) is what a
// stat gets right on every platform steps runs on.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil || info == nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// terminalReadMu serializes terminal prompts. Two steps of one job can be
// asking at the same moment (in_parallel:, across:), and two goroutines
// reading one stdin would each get half of what somebody typed.
//
//nolint:gochecknoglobals // a lock on a process-wide resource; stdin is one
var terminalReadMu sync.Mutex

// promptOnTerminal prints the question and reads one line of answer.
//
// It cannot be cancelled — a blocking read of stdin has no context — so it
// returns through a channel the caller may abandon. That is deliberate and
// bounded: at worst one goroutine sits here until somebody presses enter, on a
// question the run has already resolved some other way, and the answer it
// eventually reads is refused by the row rather than applied to it.
func promptOnTerminal(ctx context.Context, question store.Question) (string, bool) {
	terminalReadMu.Lock()
	defer terminalReadMu.Unlock()

	// Re-checked after the lock: while this goroutine waited its turn behind
	// another question, its own may have been answered elsewhere. Prompting
	// for it now would ask somebody something nobody is waiting for.
	if ctx.Err() != nil {
		return "", false
	}

	fmt.Printf("question %d> ", question.ID)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')

	answer := strings.TrimSpace(line)
	if answer == "" || (err != nil && answer == "") {
		return "", false
	}

	return answer, true
}

// terminalAnswerer is the audit record's "who" for an answer typed at this
// terminal. Deliberately not an authorization check — it records who ran the
// command on this host, which is what somebody reconstructing a decision later
// needs. Same chain as `steps approve`'s, for the same reason.
func terminalAnswerer() string {
	for _, key := range []string{"STEPS_APPROVER", "USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}

	return "terminal"
}
