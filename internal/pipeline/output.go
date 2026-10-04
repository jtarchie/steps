package pipeline

// What a task's command prints, recorded as it prints it (steps#202). The recorder sits between the runner and whatever renderer the caller installed: every byte still reaches the terminal, prefixed as before, and is also cut into chunks published while the step runs and folded into one bounded record published when it ends. Concourse's writer is the model for the chunking (atc/engine/log_event_writer.go): flush through the last newline, hold the partial line, hold a line nobody is finishing for at most a second. The differences are deliberate and steps#202 says why: a chunk is split at chunkBytes because a stored row truncates past store.MaxEventTextBytes, and a carriage return is collapsed here rather than replayed by a terminal emulator the browser does not have.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
)

const (
	// chunkBytes bounds one published chunk; a line longer than this is split.
	chunkBytes = 8_000
	// chunkHold is how long a partial line waits for its newline before it is published anyway — Concourse's one second, checked on a write, so a step that goes quiet mid-line shows that line on its next byte, not on a timer.
	chunkHold = time.Second
	// publishedHeadBytes is how much of the start of a long output the record keeps; the rest of maxPublishedOutputBytes is its end, where a failure explains itself.
	publishedHeadBytes = 8_000
)

// outputRecorder is one task command's output: two chunkers, one per stream, feeding one record in arrival order.
type outputRecorder struct {
	stdout, stderr *chunker
	flushes        []func()
	record         headTail
	mu             sync.Mutex
}

// recordTaskOutput installs a recorder over ctx's output for a command about to run under label, returning the ctx to run it with. finish publishes the record.
func recordTaskOutput(ctx context.Context, label string) (context.Context, *outputRecorder) {
	rec := &outputRecorder{}

	ctx = events.Tee(ctx, func(stderr bool, inner io.Writer) io.Writer {
		stream := "stdout"
		if stderr {
			stream = "stderr"
		}

		chunks := newChunker(time.Now, func(text string) {
			rec.mu.Lock()
			rec.record.write(text)
			rec.mu.Unlock()

			publishForCurrentStep(ctx, events.TypeStepOutputChunk, stream, text)
		})

		// The runner is handed no label so the chunks are the command's own bytes; the terminal still gets its prefix here.
		prefixed, flush := shell.NewPrefixedStream(label, inner)
		rec.flushes = append(rec.flushes, flush)

		if stderr {
			rec.stderr = chunks
		} else {
			rec.stdout = chunks
		}

		return io.MultiWriter(prefixed, chunks)
	})

	return ctx, rec
}

// finish publishes the record once the command has exited: whatever the chunkers still hold, then the whole as one output event, which the store takes as the signal to drop the chunks.
func (r *outputRecorder) finish(ctx context.Context) {
	r.stdout.close()
	r.stderr.close()

	for _, flush := range r.flushes {
		flush()
	}

	r.mu.Lock()
	text := r.record.String()
	r.mu.Unlock()

	publishForCurrentStep(ctx, events.TypeStepOutput, "", strings.TrimRight(text, "\n"))
}

// chunker cuts one stream into published pieces.
type chunker struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	last time.Time
	now  func() time.Time
	emit func(string)
	// midLine is whether the last chunk ended without a newline, so a carriage return opening the next one is a line starting over.
	midLine bool
}

func newChunker(now func() time.Time, emit func(string)) *chunker {
	return &chunker{now: now, emit: emit, last: now()}
}

func (c *chunker) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buf.Write(p)

	held := c.buf.Bytes()

	switch cut := bytes.LastIndexByte(held, '\n'); {
	case cut >= 0:
		c.flush(cut + 1)
	case c.buf.Len() >= chunkBytes, c.now().Sub(c.last) >= chunkHold:
		c.flush(c.buf.Len())
	}

	return len(p), nil
}

func (c *chunker) close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.flush(c.buf.Len())
}

// flush publishes the first n held bytes in pieces of at most chunkBytes, cut at a newline when one is in reach.
func (c *chunker) flush(n int) {
	if n == 0 {
		return
	}

	c.last = c.now()
	piece := c.buf.Next(n)

	for len(piece) > 0 {
		cut := len(piece)
		if cut > chunkBytes {
			cut = chunkBytes
			if nl := bytes.LastIndexByte(piece[:cut], '\n'); nl > 0 {
				cut = nl + 1
			}
		}

		text := collapseCR(string(piece[:cut]), c.midLine)
		piece = piece[cut:]

		if text == "" {
			continue
		}

		c.midLine = text[len(text)-1] != '\n'
		c.emit(text)
	}
}

// collapseCR keeps, of every line, what a terminal would show after its carriage returns: the text after the last one. A chunk that opens with a return while the previous one left a line unfinished starts a new line instead, since the line it would overwrite has already been published.
func collapseCR(text string, midLine bool) string {
	if !strings.Contains(text, "\r") {
		return text
	}

	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		body, nl := strings.CutSuffix(line, "\n")

		body = strings.TrimSuffix(body, "\r")
		if cr := strings.LastIndexByte(body, '\r'); cr >= 0 {
			body = body[cr+1:]
		}

		if nl {
			body += "\n"
		}

		lines[i] = body
	}

	collapsed := strings.Join(lines, "")
	if midLine && strings.HasPrefix(text, "\r") && collapsed != "" {
		collapsed = "\n" + collapsed
	}

	return collapsed
}

// headTail keeps the first publishedHeadBytes and the last of what fits in maxPublishedOutputBytes, and counts the rest.
type headTail struct {
	head  []byte
	tail  []byte
	total int
}

func (h *headTail) write(text string) {
	h.total += len(text)

	if room := publishedHeadBytes - len(h.head); room > 0 {
		take := min(room, len(text))
		h.head = append(h.head, text[:take]...)
		text = text[take:]
	}

	tailMax := maxPublishedOutputBytes - publishedHeadBytes
	h.tail = append(h.tail, text...)

	// Trimmed only once well past the bound, so a chatty step costs a copy per tailMax bytes rather than per write.
	if len(h.tail) > 2*tailMax {
		h.tail = append([]byte(nil), h.tail[len(h.tail)-tailMax:]...)
	}
}

func (h *headTail) String() string {
	tailMax := maxPublishedOutputBytes - publishedHeadBytes

	tail := h.tail
	if len(tail) > tailMax {
		tail = tail[len(tail)-tailMax:]
		if nl := bytes.IndexByte(tail, '\n'); nl >= 0 && nl < len(tail)-1 {
			tail = tail[nl+1:]
		}
	}

	elided := h.total - len(h.head) - len(tail)
	if elided <= 0 {
		return string(h.head) + string(tail)
	}

	marker := fmt.Sprintf("... [elided %d bytes]\n", elided)
	if !bytes.HasSuffix(h.head, []byte("\n")) {
		marker = "\n" + marker
	}

	return string(h.head) + marker + string(tail)
}
