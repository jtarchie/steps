package pipeline

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func collecting() (*[]string, func(string)) {
	var got []string

	return &got, func(text string) { got = append(got, text) }
}

// TestChunkerFlushesWholeLines: Concourse's rule — through the last newline, the partial line held — so a reader never sees half a line that the next byte finishes.
func TestChunkerFlushesWholeLines(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{at: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	got, emit := collecting()
	c := newChunker(clock.now, emit)

	_, _ = c.Write([]byte("one\ntwo\nthr"))
	_, _ = c.Write([]byte("ee\n"))

	if want := []string{"one\ntwo\n", "three\n"}; strings.Join(*got, "|") != strings.Join(want, "|") {
		t.Errorf("chunks = %q, want %q", *got, want)
	}
}

// TestChunkerHoldsAPartialLineForASecond: a line nobody finishes is published anyway once a second has passed since the last flush — checked on the write, as Concourse does, never by a timer — and close publishes whatever is left.
func TestChunkerHoldsAPartialLineForASecond(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{at: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	got, emit := collecting()
	c := newChunker(clock.now, emit)

	_, _ = c.Write([]byte("building."))
	clock.at = clock.at.Add(500 * time.Millisecond)
	_, _ = c.Write([]byte("."))

	if len(*got) != 0 {
		t.Fatalf("published %q before the hold was up", *got)
	}

	clock.at = clock.at.Add(600 * time.Millisecond)
	_, _ = c.Write([]byte("."))

	if want := "building..."; strings.Join(*got, "|") != want {
		t.Errorf("chunks = %q, want %q", *got, want)
	}

	_, _ = c.Write([]byte(" done"))
	c.close()

	if want := "building...| done"; strings.Join(*got, "|") != want {
		t.Errorf("chunks = %q, want %q", *got, want)
	}
}

// TestChunkerSplitsWhatAStoredRowCouldNotHold: one chunk is one row, and a row truncates past store.MaxEventTextBytes; a line that long is split, and a run of short lines is cut at a line boundary.
func TestChunkerSplitsWhatAStoredRowCouldNotHold(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{at: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	got, emit := collecting()
	c := newChunker(clock.now, emit)

	var lines strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&lines, "line %04d of the build log\n", i)
	}

	_, _ = c.Write([]byte(lines.String()))

	total := 0

	for _, chunk := range *got {
		total += len(chunk)

		if len(chunk) > chunkBytes {
			t.Errorf("a chunk of %d bytes outgrew its row", len(chunk))
		}

		if !strings.HasSuffix(chunk, "\n") {
			t.Errorf("a chunk was cut mid-line: %q", chunk[len(chunk)-20:])
		}
	}

	if total != lines.Len() {
		t.Errorf("%d bytes published of %d written", total, lines.Len())
	}

	got, emit = collecting()
	c = newChunker(clock.now, emit)
	_, _ = c.Write([]byte(strings.Repeat("x", 2*chunkBytes+1) + "\n"))

	if len(*got) != 3 || len((*got)[0]) != chunkBytes {
		t.Errorf("a line with no newline to cut at came out as %d chunks, want three of at most %d bytes", len(*got), chunkBytes)
	}
}

// TestChunkerCollapsesCarriageReturns: a progress bar's returns leave the value a terminal would show when the hold is up, and a return that opens the next chunk starts a line rather than gluing onto the one already published.
func TestChunkerCollapsesCarriageReturns(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{at: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	got, emit := collecting()
	c := newChunker(clock.now, emit)

	_, _ = c.Write([]byte("fetch\r\n10%\r20%\r30%"))
	clock.at = clock.at.Add(2 * time.Second)
	_, _ = c.Write([]byte("\r40%\r50%"))
	clock.at = clock.at.Add(2 * time.Second)
	_, _ = c.Write([]byte("\r100%\ndone\n"))

	if want := "fetch\n|50%|\n100%\ndone\n"; strings.Join(*got, "|") != want {
		t.Errorf("chunks = %q, want %q", *got, want)
	}
}

// TestHeadTailKeepsBothEndsInOrder: the record is how the step started and how it ended, with the cut named, because the last line is where a long build says why it failed — and a short output is kept whole, with no marker.
func TestHeadTailKeepsBothEndsInOrder(t *testing.T) {
	t.Parallel()

	var short headTail

	short.write("hello\n")
	short.write("world\n")

	if got := short.String(); got != "hello\nworld\n" {
		t.Errorf("short = %q, want it whole", got)
	}

	var long headTail

	for i := range 20_000 {
		long.write(fmt.Sprintf("%d\n", i))
	}

	got := long.String()

	switch {
	case !strings.HasPrefix(got, "0\n1\n2\n"):
		t.Errorf("record does not start at the start: %.40q", got)
	case !strings.HasSuffix(got, "19998\n19999\n"):
		t.Errorf("record does not end at the end: %q", got[len(got)-40:])
	case !strings.Contains(got, "... [elided "):
		t.Error("record does not name its cut")
	case len(got) > maxPublishedOutputBytes+100:
		t.Errorf("record is %d bytes, want about %d", len(got), maxPublishedOutputBytes)
	}

	at := strings.Index(got, "... [elided ")
	marker := got[at : at+strings.Index(got[at:], "]")+1]

	var elided int

	_, err := fmt.Sscanf(marker, "... [elided %d bytes]", &elided)
	if err != nil {
		t.Fatalf("marker %q: %v", marker, err)
	}

	kept := len(got) - len(marker) - 1
	if kept+elided != long.total {
		t.Errorf("kept %d + elided %d != written %d", kept, elided, long.total)
	}
}

// TestChunkerFillsARowBeforeCuttingAtALeadingNewline: a newline at the very start of an over-long piece is no place to cut — it would spend a whole row on one byte.
func TestChunkerFillsARowBeforeCuttingAtALeadingNewline(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{at: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	got, emit := collecting()
	c := newChunker(clock.now, emit)

	_, _ = c.Write([]byte("\n" + strings.Repeat("x", chunkBytes+1000) + "\n"))

	if len(*got) != 2 || len((*got)[0]) != chunkBytes {
		t.Errorf("%d chunks, first of %d bytes; want two, the first a full %d", len(*got), len((*got)[0]), chunkBytes)
	}
}

// TestHeadTailKeepsAnOutputThatExactlyFits: an output exactly the size of the record is kept whole, not cut and marked.
func TestHeadTailKeepsAnOutputThatExactlyFits(t *testing.T) {
	t.Parallel()

	line := strings.Repeat("y", 99) + "\n"
	whole := strings.Repeat(line, maxPublishedOutputBytes/len(line))

	if len(whole) != maxPublishedOutputBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(whole), maxPublishedOutputBytes)
	}

	var h headTail
	for i := 0; i < len(whole); i += len(line) {
		h.write(whole[i : i+len(line)])
	}

	if got := h.String(); got != whole {
		t.Errorf("an output that fits was not kept whole: %d bytes, marker at %d", len(got), strings.Index(got, "... [elided"))
	}
}

// TestHeadTailStartsItsTailOnAWholeLine: wherever the byte bound lands — mid-line, on a newline, on a line's first byte — what follows the marker is a line the step printed, never the back half of one or a blank.
func TestHeadTailStartsItsTailOnAWholeLine(t *testing.T) {
	t.Parallel()

	for pad := range 6 {
		var h headTail

		for i := range 10_000 {
			h.write(fmt.Sprintf("%05d\n", i))
		}

		h.write(strings.Repeat("z", pad) + "\n")

		got := h.String()

		at := strings.Index(got, "]\n")
		if at < 0 {
			t.Fatalf("pad %d: no marker in %d bytes", pad, len(got))
		}

		first, _, _ := strings.Cut(got[at+2:], "\n")
		if len(first) != 5 || strings.Trim(first, "0123456789") != "" {
			t.Errorf("pad %d: tail opens with %q, want a whole printed line", pad, first)
		}
	}
}

// TestHeadTailKeepsTheEndOfOneLongLine: a tail that is the end of one line has no line boundary to move to, so it is kept as it is rather than dropped.
func TestHeadTailKeepsTheEndOfOneLongLine(t *testing.T) {
	t.Parallel()

	var h headTail

	h.write(strings.Repeat("h", publishedHeadBytes))
	h.write(strings.Repeat("l", maxPublishedOutputBytes) + "end\n")

	got := h.String()
	if !strings.HasSuffix(got, strings.Repeat("l", 1000)+"end\n") {
		t.Errorf("the long last line was not kept: record ends %q", got[max(0, len(got)-40):])
	}
}
