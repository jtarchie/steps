package shell

import (
	"bytes"
	"io"
	"strings"
)

// prefixedWriter wraps dst, prepending prefix at the start of every line
// written through it — so output from several shell-backed steps sharing one
// terminal (task run:, resource check/in/out, an agent's run_shell/custom
// tools) stays attributable to whichever step produced it, the same idea as
// onsi/gomega/gexec's PrefixedWriter.
//
// Crucially it does NOT buffer to line boundaries: every byte is forwarded to
// dst as it arrives, with the prefix inserted only when a new line begins.
// This matters because the streamed content it's used for — an agent's
// live token-by-token model output, run_shell command output — often arrives
// as long, newline-sparse chunks; an earlier line-buffering version held all
// of it until a \n (or the final flush), so nothing appeared to stream at all
// until a line completed. atLineStart tracks whether the next byte begins a
// line (true initially, and immediately after each \n written), which is the
// only state needed to prefix correctly while streaming.
type prefixedWriter struct {
	prefix      string
	dst         io.Writer
	atLineStart bool
}

func newPrefixedWriter(prefix string, dst io.Writer) *prefixedWriter {
	return &prefixedWriter{prefix: prefix, dst: dst, atLineStart: true}
}

func (p *prefixedWriter) Write(b []byte) (int, error) {
	n := len(b)

	for len(b) > 0 {
		if p.atLineStart {
			_, err := io.WriteString(p.dst, p.prefix)
			if err != nil {
				return n, err //nolint:wrapcheck // passes the underlying writer's own error through unchanged
			}

			p.atLineStart = false
		}

		idx := bytes.IndexByte(b, '\n')
		if idx < 0 {
			// No newline in what's left: stream it all now, stay mid-line.
			_, err := p.dst.Write(b)
			if err != nil {
				return n, err //nolint:wrapcheck // passes the underlying writer's own error through unchanged
			}

			break
		}

		// Stream through the newline; the next byte begins a fresh line.
		_, err := p.dst.Write(b[:idx+1])
		if err != nil {
			return n, err //nolint:wrapcheck // passes the underlying writer's own error through unchanged
		}

		p.atLineStart = true
		b = b[idx+1:]
	}

	return n, nil
}

// flush terminates a trailing, un-newline-terminated line so whatever prints
// next (the next task's header, a job.done log line) starts cleanly on its
// own line. The line's content has already been streamed to dst; this only
// adds the missing \n. A no-op when the last byte written was already a
// newline (the overwhelmingly common case) or nothing was written at all.
func (p *prefixedWriter) flush() {
	if p.atLineStart {
		return
	}

	_, _ = io.WriteString(p.dst, "\n")
	p.atLineStart = true
}

// prefixedStream returns dst unchanged and a no-op flush when label is ""
// (byte-identical to before labeling existed — the default for every
// caller that never opts in via WithLabel), or a prefixedWriter over dst
// and its flush method when label is set.
func prefixedStream(label string, dst io.Writer) (w io.Writer, flush func()) {
	if label == "" {
		return dst, func() {}
	}

	pw := newPrefixedWriter("["+label+"] ", dst)

	return pw, pw.flush
}

// PrefixLines prepends "[label] " to every line of s (splitting on \n and
// rejoining, so a trailing partial line — s not ending in \n — is prefixed
// too without gaining a synthetic newline it didn't have). Used for output
// that's captured in full before being printed (runAssertedTask/runFixTask's
// post-hoc printTaskOutput), as opposed to prefixedWriter's live-streaming
// case. Returns s unchanged when label is "" or s is empty.
func PrefixLines(label, s string) string {
	if label == "" || s == "" {
		return s
	}

	prefix := "[" + label + "] "

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			continue // s ended in \n; leave the empty tail segment alone
		}

		lines[i] = prefix + line
	}

	return strings.Join(lines, "\n")
}
