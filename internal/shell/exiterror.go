package shell

// A command's own nonzero exit, reported from somewhere os/exec cannot see.

import "fmt"

// ExitError is a command that started and then exited nonzero (or was killed
// by a signal) somewhere os/exec cannot see it: on a worker at the far end of
// a venue, or in a container this process talks to over the engine API.
//
// It exists because the whole pipeline decides "the step said no" from "the
// machinery broke" by asking IsExitError, and *exec.ExitError — the only
// answer there used to be — carries an *os.ProcessState, has no constructor,
// and so can only ever describe a child of THIS process. Neither a venue nor
// a container has one. Reporting either as a plain error would classify every
// red step as infrastructure, firing on_error where the pipeline author wrote
// on_failure.
//
// The inverse matters just as much and is why a venue must NOT reach for this
// type when a command never ran at all: guard.go turns that case into "guard
// command could not run" and fails the step, where an ExitError would let an
// unreachable worker read as "the guard said false" and silently skip work.
type ExitError struct {
	// Command is the shell string that failed, for the message.
	Command string
	// Venue names the machine, because a red build on a fleet that does not
	// say which box sends the operator to look at the wrong one. Empty for a
	// container on the daemon this process is already talking to, where the
	// caller names the image instead.
	Venue string
	// Code is the exit status, or -1 for a signalled command — the same
	// sentinel exitCodeOf reports for a local kill, so the two are
	// indistinguishable to every caller downstream.
	Code int
}

func (e *ExitError) Error() string {
	if e.Venue == "" {
		return fmt.Sprintf("command %q exited with status %d", e.Command, e.Code)
	}

	return fmt.Sprintf("command %q on worker %q exited with status %d", e.Command, e.Venue, e.Code)
}

// ExitCode reports the status the command exited with. Named to match
// *exec.ExitError's own method, which is what lets exitCodeOf ask for the
// capability rather than the concrete type.
func (e *ExitError) ExitCode() int { return e.Code }

// SignalledExitCode is the status a command killed by a signal reports.
//
// os/exec answers -1 for a locally signalled process, and the venue carries
// the same sentinel across the wire so the two are indistinguishable to
// everything downstream. Named because two callers now have to ASK the
// question — "did this command choose its status, or was it killed?" — and a
// bare -1 at a call site says neither.
const SignalledExitCode = -1
