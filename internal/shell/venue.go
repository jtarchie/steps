package shell

// What a venue needs from this package in order to run a command somewhere
// else and have it mean the same thing.
//
// Each export here exists because the alternative is a second copy. A venue
// that bounded output on its own terms, or prefixed lines on its own terms, or
// decided for itself what a cancelled command looks like, would make a step
// that moved to a worker start reporting differently — which is exactly the
// class of difference this feature is supposed not to have.

import (
	"context"
	"io"
)

// Capture accumulates one stream of a command's output under the same rules
// RunCaptureFullLimited applies: unbounded when maxBytes is zero or less,
// truncated with a trailing marker when spillDir is empty, spilled to a file
// under spillDir otherwise.
//
// The writer stays on the orchestrator even when the command does not. A spill
// file names a path a later read_file resolves HERE, so writing it on the
// worker would hand a model a pointer to a file it cannot open.
type Capture struct {
	writer captureWriter
}

// NewCapture returns a Capture applying the policy maxBytes and spillDir
// describe.
func NewCapture(maxBytes int, spillDir string) *Capture {
	return &Capture{writer: newCaptureWriter(maxBytes, spillDir)}
}

func (c *Capture) Write(p []byte) (int, error) {
	return c.writer.Write(p) //nolint:wrapcheck // a pass-through to the writer this type exists to expose
}

// Result is everything the stream produced, bounded as configured.
func (c *Capture) Result() string { return c.writer.result() }

// NewPrefixedStream is prefixedStream: a writer that stamps each line with
// "[label] " on its way to dst, and the flush that emits a trailing partial
// line. A venue streams a worker's output through this so a placed step's
// lines look like every other step's.
func NewPrefixedStream(label string, dst io.Writer) (w io.Writer, flush func()) {
	return prefixedStream(label, dst)
}

// WrapIfCanceled is wrapIfCanceled: it makes err's chain satisfy
// errors.Is(_, ctx.Err()) when ctx was cancelled by the time the command
// finished.
//
// A venue needs it for the same reason HostRunner does, and cannot infer it
// from the wire: a cancelled remote command reports a signal death, which is
// indistinguishable from a command that died on its own. Only the context
// knows which happened.
func WrapIfCanceled(ctx context.Context, err error) error {
	return wrapIfCanceled(ctx, err)
}
