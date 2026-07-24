package shell

import (
	"bytes"
	"io"
	"strings"
)

// prefixedWriter wraps dst, prepending prefix to every complete line written
// through it — so output from several shell-backed steps sharing one
// terminal (task run:, resource check/in/out, an agent's run_shell/custom
// tools) stays attributable to whichever step produced it, the same idea as
// onsi/gomega/gexec's PrefixedWriter. A partial (not yet newline-terminated)
// line is buffered across Write calls rather than prefixed mid-line; flush
// emits whatever's left buffered once the command producing it exits, so a
// stream whose last line never ends in \n isn't silently dropped.
type prefixedWriter struct {
	prefix string
	dst    io.Writer
	buf    bytes.Buffer
}

func newPrefixedWriter(prefix string, dst io.Writer) *prefixedWriter {
	return &prefixedWriter{prefix: prefix, dst: dst}
}

func (p *prefixedWriter) Write(b []byte) (int, error) {
	n := len(b)

	p.buf.Write(b)

	for {
		idx := bytes.IndexByte(p.buf.Bytes(), '\n')
		if idx < 0 {
			break
		}

		_, werr := io.WriteString(p.dst, p.prefix)
		if werr != nil {
			return n, werr //nolint:wrapcheck // passes the underlying writer's own error through unchanged
		}

		_, werr = p.dst.Write(p.buf.Next(idx + 1))
		if werr != nil {
			return n, werr //nolint:wrapcheck // passes the underlying writer's own error through unchanged
		}
	}

	return n, nil
}

// flush emits any buffered partial line, with a trailing newline added for
// terminal readability. A no-op when nothing is buffered (the overwhelmingly
// common case: well-behaved command output ends every line in \n).
func (p *prefixedWriter) flush() {
	if p.buf.Len() == 0 {
		return
	}

	_, _ = io.WriteString(p.dst, p.prefix+p.buf.String()+"\n")
	p.buf.Reset()
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
