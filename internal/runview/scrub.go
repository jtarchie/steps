package runview

import (
	"strings"
	"unicode/utf8"

	"github.com/jtarchie/steps/internal/events"
)

// ScrubEvent makes a recorded event safe to print to somebody else's terminal: its text can come from untrusted input — a PR under review — and an OSC sequence in it writes the viewer's clipboard or retitles their window.
//
// Every printed field, not only the free-text ones: a job name is unvalidated YAML, and the pipeline can come from the same untrusted branch.
func ScrubEvent(event events.Event) events.Event {
	event.Job = Scrub(event.Job)
	event.StepKind = Scrub(event.StepKind)
	event.Status = Scrub(event.Status)
	event.Text = Scrub(event.Text)
	event.Name = Scrub(event.Name)
	event.Detail = Scrub(event.Detail)
	event.StepName = Scrub(event.StepName)
	event.Worker = Scrub(event.Worker)

	return event
}

// Scrub drops every escape sequence and control character but tab and newline, with web.frameLines' rule for carriage returns.
func Scrub(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var out strings.Builder

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size

		switch {
		case r == 0x1b:
			i = skipEscape(text, i)
		case r == '\t' || r == '\n':
			out.WriteRune(r)
		case r == utf8.RuneError && size == 1, r < 0x20, r >= 0x7f && r < 0xa0:
		default:
			out.WriteRune(r)
		}
	}

	return out.String()
}

// skipEscape returns where the sequence introduced by an ESC just before i ends.
func skipEscape(text string, i int) int {
	if i >= len(text) {
		return i
	}

	switch text[i] {
	case '[':
		return skipCSI(text, i+1)
	case ']', 'P', 'X', '^', '_':
		return skipString(text, i+1)
	default:
		return i + 1
	}
}

// skipCSI passes parameters and intermediates, then one final byte. A byte no CSI can hold ends it unconsumed, so a stray "ESC [" cannot eat the lines after it.
func skipCSI(text string, i int) int {
	for ; i < len(text); i++ {
		switch c := text[i]; {
		case c >= 0x40 && c <= 0x7e:
			return i + 1
		case c < 0x20 || c > 0x7e:
			return i
		}
	}

	return i
}

// skipString passes an OSC or one of the other string sequences, which run to BEL or ST — or, here, a newline, so an unterminated one hides one line rather than every line after it.
func skipString(text string, i int) int {
	for ; i < len(text); i++ {
		if text[i] == 0x07 {
			return i + 1
		}

		if text[i] == '\n' {
			return i
		}

		if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '\\' {
			return i + 2
		}
	}

	return i
}
