package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jtarchie/steps/internal/runview"
)

// table is the aligned-column writer every listing prints through. A cell can hold text an untrusted PR chose — an approval's message, an agent's question, a run's error — so each row is scrubbed on its way in, before tabwriter measures it.
type table struct{ *tabwriter.Writer }

func (t table) Write(p []byte) (int, error) {
	_, err := t.Writer.Write([]byte(runview.Scrub(string(p))))
	if err != nil {
		return 0, err //nolint:wrapcheck // an io.Writer passes its writer's error through
	}

	return len(p), nil
}

func newTabWriter() table {
	return table{tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)}
}

func flush(writer table) error {
	err := writer.Flush()
	if err != nil {
		return fmt.Errorf("could not write output: %w", err)
	}

	return nil
}

// formatWhen renders a timestamp in local time, or "-" when unset.
func formatWhen(when time.Time) string {
	if when.IsZero() {
		return "-"
	}

	return when.Local().Format("2006-01-02 15:04:05")
}

// stampSeconds and stampNano print a time in the UTC layout sqlite stores its
// column in — whole seconds for approvals and the breaker, a fixed nine-digit
// fraction for questions and memories — which is what these listings printed
// back when a row carried that stored text rather than a time.Time.
func stampSeconds(when time.Time) string { return when.UTC().Format(time.RFC3339) }

func stampNano(when time.Time) string {
	return when.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

// firstLine truncates an error to its first line and a readable width, since
// these are columns in a table — the full text is in the run's own output.
func firstLine(text string) string {
	if text == "" {
		return "-"
	}

	line, _, _ := strings.Cut(text, "\n")

	const maxWidth = 70
	if len(line) > maxWidth {
		return line[:maxWidth-1] + "…"
	}

	return line
}

// maxStatusWidth is how wide the STATUS cell may get before it is elided —
// the same budget firstLine spends on the error columns of the other tables.
const maxStatusWidth = 70

// elideMiddle drops the middle of an over-long reason rather than its tail, fitting it to the STATUS column.
//
// Both ends carry meaning and neither survives the other's loss: a dial
// failure names what was attempted first ("Post https://…") and how it went
// last ("connection refused"), and truncating from the right — which is what
// every other column here does, where the head is the whole content — keeps
// only the URL nobody was asking about. Runes, not bytes: half a rune in a
// tabwriter cell miscounts the column as well as printing as garbage.
func elideMiddle(text string) string {
	runes := []rune(text)
	if len(runes) <= maxStatusWidth {
		return text
	}

	head := maxStatusWidth / 3

	return string(runes[:head]) + "…" + string(runes[len(runes)-(maxStatusWidth-head-1):])
}

// pluralize adds the English plural s, so a count reads as a phrase.
func pluralize(count int, noun string) string {
	if count == 1 {
		return noun
	}

	return noun + "s"
}

func truncateName(name string, width int) string {
	if len(name) <= width {
		return name
	}

	return name[:width-1] + "…"
}
