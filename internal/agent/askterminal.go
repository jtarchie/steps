package agent

// The terminal channel: putting a question to whoever is sitting at the
// terminal that started the run.
//
// The shape here is decided by one fact: a read of stdin cannot be cancelled.
// A prompt that gave up would otherwise sit in that read forever, so the read
// belongs to ONE long-lived goroutine that outlives any individual question,
// and a prompt is a select over what that goroutine delivers. Everything else
// — one reader per prompt, a lock held across the read — leaks a goroutine per
// question and, worse, strands whatever it holds when a question is answered
// somewhere else.

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
// is at the other end of stdin — which is every CI run, every `steps web`
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

// terminalReader is the process's single stdin reader: started once, on the
// first prompt, and never stopped — there is no way to stop it, which is the
// whole reason it is one goroutine rather than one per question.
//
//nolint:gochecknoglobals // a handle on a process-wide resource; stdin is one
var terminalReader struct {
	once  sync.Once
	lines chan string
	// prompt serializes who is being asked. Two steps of one job can ask at
	// the same moment (in_parallel:, across:), and two prompts printed over
	// each other would leave a person answering they cannot tell which. Held
	// only while a prompt is actually waiting, and released the moment its
	// question resolves — by any channel — because the wait is a select on
	// context rather than a blocking read.
	prompt sync.Mutex
}

// terminalLines starts the reader on first use and returns its channel.
//
// UNBUFFERED, deliberately: the goroutine blocks on the send until some prompt
// takes the line, so a line typed ahead of the next question is delivered to
// that question rather than dropped — which is how a terminal behaves and what
// a per-prompt bufio.Reader could not do, since each one discarded whatever it
// had read past the first newline.
func terminalLines() <-chan string {
	terminalReader.once.Do(func() {
		terminalReader.lines = make(chan string)

		go func() {
			reader := bufio.NewReader(os.Stdin)

			for {
				line, err := reader.ReadString('\n')

				answer := strings.TrimSpace(line)
				if answer != "" {
					terminalReader.lines <- answer
				}

				if err != nil {
					return
				}
			}
		}()
	})

	return terminalReader.lines
}

// promptOnTerminal prints the question and waits for a line.
//
// It returns as soon as ctx is done, which is what makes an abandoned prompt
// cost nothing: the caller cancels when the question resolves by any route, so
// the lock is released and this goroutine exits while the reader stays put,
// still holding whatever was typed for whoever asks next.
func promptOnTerminal(ctx context.Context, question store.Question) (string, bool) {
	lines := terminalLines()

	if !lockTerminal(ctx) {
		return "", false
	}

	defer terminalReader.prompt.Unlock()

	// Re-checked under the lock: while this prompt waited its turn behind
	// another question, its own may have been answered elsewhere. Prompting for
	// it now would ask somebody something nobody is waiting for.
	if ctx.Err() != nil {
		return "", false
	}

	fmt.Printf("question %d> ", question.ID)

	select {
	case answer := <-lines:
		return answer, answer != ""
	case <-ctx.Done():
		return "", false
	}
}

// lockTerminal takes the prompt lock without giving up cancellability — a
// question resolved while queued behind another must not have to wait for that
// one to finish before its own call can return.
func lockTerminal(ctx context.Context) bool {
	acquired := make(chan struct{})

	go func() {
		terminalReader.prompt.Lock()
		close(acquired)
	}()

	select {
	case <-acquired:
		return true
	case <-ctx.Done():
		// The lock is still being waited for by that goroutine; when it lands,
		// nothing holds it open — the deferred Unlock below never runs, so
		// release it there instead.
		go func() {
			<-acquired
			terminalReader.prompt.Unlock()
		}()

		return false
	}
}

// terminalAnswerer is the audit record's "who" for an answer typed at this
// terminal. Deliberately not an authorization check — it records who ran the
// command on this host, which is what somebody reconstructing a decision later
// needs. Same chain as `steps approvals approve`'s, for the same reason.
func terminalAnswerer() string {
	for _, key := range []string{"STEPS_APPROVER", "USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}

	return "terminal"
}
