package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
)

// TestForEachCarrySkipAlignment: with carry: true and onItemFailure: "skip",
// the aggregate items pair each surviving output with its ORIGINAL source item
// and index — so a downstream state stays correctly aligned even though the
// failed item was dropped (the misalignment a hand-written items[i] zip would
// silently produce).
func TestForEachCarrySkipAlignment(t *testing.T) {
	wf := `
const fan = {
  forEach: { over: ({ names }) => names, as: "name", carry: true, onItemFailure: "skip" },
  retry: "none",
  prompt: ({ name }) => "greet " + name,
  output: { greeting: "string" },
};
const report = {
  prompt: ({ fan }) => fan.items.map((e) => e.index + ":" + e.item + "=" + e.output.greeting).join(","),
};
export default {
  name: "carry",
  input: { names: { type: "array", required: true } },
  model: "mock",
  states: { fan, report },
  flow: pipe(fan, report, done),
};`
	// Three items; the middle one errors and is skipped. report should see the
	// first and third paired with indices 0 and 2 (not 0 and 1).
	script := `
fan:
  - text: "{\"greeting\": \"hi-ann\"}"
  - error: provider_error
  - text: "{\"greeting\": \"hi-cass\"}"
report:
  - text: "ok"
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newTestEngine(t, writeScript(t, script))
	rec := &recorder{}
	eng.Listener = rec

	res, err := eng.Start(context.Background(), m, map[string]any{
		"names": []any{"ann", "bob", "cass"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	msgs := rec.user["report"]
	if len(msgs) != 1 {
		t.Fatalf("report prompts = %v, want one", msgs)
	}
	want := "0:ann=hi-ann,2:cass=hi-cass"
	if msgs[0] != want {
		t.Errorf("report prompt = %q, want %q (skip-safe carry alignment)", msgs[0], want)
	}
	if strings.Contains(msgs[0], "bob") {
		t.Errorf("report prompt %q includes the skipped item", msgs[0])
	}
}
