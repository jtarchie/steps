package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// frameLines cuts a fragment into the data lines one SSE message carries.
//
// html/template escapes the five HTML metacharacters and NOT the carriage
// return, but SSE ends a line on CRLF, CR *or* LF — so a lone CR in a step's
// output (every progress bar writes them) ends the data line early and the
// markup after it is read as SSE fields. `\r\revent: done\rdata: 0\r\r` on a
// task's stdout is a forged run-completion frame: it closes the reader's
// stream and paints the tab red on a job that is still running.
//
// Every line ending becomes a data line, and a lone CR is a line ending too —
// not because SSE says so but because HTML does: the parser turns every CR
// in a page's bytes into LF before it tokenizes, so on reload a progress
// bar's overwrites are three lines. Sent as the entity instead, the CR
// survived into the live DOM as U+000D, which CSS draws as a space, and the
// reader watched one run-on line become three at the closing reload.
func frameLines(html string) []string {
	normalized := strings.ReplaceAll(html, "\r\n", "\n")

	return strings.Split(strings.ReplaceAll(normalized, "\r", "\n"), "\n")
}

// writeFrame emits one HTML fragment as an unnamed SSE message, which is the
// shape hx-sse swaps. Named messages dispatch a DOM event instead, which is
// what `done` is for.
func writeFrame(response http.ResponseWriter, id int64, html string) {
	_, _ = fmt.Fprintf(response, "id: %d\n", id)

	// One data: line per line of markup. SSE rejoins them with newlines, so a
	// <pre> block arrives with its whitespace intact — the whole point of
	// sending markup the server already escaped.
	for _, line := range frameLines(html) {
		_, _ = fmt.Fprintf(response, "data: %s\n", line)
	}

	_, _ = fmt.Fprint(response, "\n")
	flush(response)
}

// writeSSE emits one named event carrying JSON. A marshal failure is skipped
// rather than killing the stream: one unrenderable event must not end the
// run's live view.
func writeSSE(response http.ResponseWriter, name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	_, _ = fmt.Fprintf(response, "event: %s\ndata: %s\n\n", name, data)
	flush(response)
}

// flush pushes a frame to the reader now rather than when the buffer fills, which is the whole of what makes the stream live.
//
// A dropped error rather than echo's Response.Flush, which PANICS on a writer that cannot flush: every writer this sees can, and a live view is not worth a 500 if one someday cannot.
func flush(response http.ResponseWriter) {
	_ = http.NewResponseController(response).Flush()
}
