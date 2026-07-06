package machine

import (
	"strings"
	"testing"
)

// TestForEachCarryStubShape: with carry, a downstream reference to a bare
// per-item field fails the load naming item/output/index, while the paired
// access (items[i].output.field) passes.
func TestForEachCarryStubShape(t *testing.T) {
	bad := `
const fan = {
  forEach: { over: ({ names }) => names, as: "name", carry: true },
  prompt: ({ name }) => "greet " + name,
  output: { greeting: "string" },
};
const report = { prompt: ({ fan }) => fan.items.map((e) => e.greeting).join(",") };
export default {
  name: "carry-typo",
  input: { names: "array" },
  model: "mock",
  states: { fan, report },
  flow: pipe(fan, report, done),
};`
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected the bare items[].greeting access to fail under carry")
	}
	if !strings.Contains(err.Error(), "item") || !strings.Contains(err.Error(), "output") {
		t.Errorf("error = %v, want it to name the carry shape (item/output/index)", err)
	}

	good := strings.Replace(bad, "e.greeting", "e.output.greeting", 1)
	_, err = Parse([]byte(good))
	if err != nil {
		t.Fatalf("paired access should load: %v", err)
	}
}

func TestForEachUnknownKey(t *testing.T) {
	src := `
const fan = {
  forEach: { over: ({ names }) => names, as: "name", onItemFail: "skip" },
  prompt: ({ name }) => "greet " + name,
};
export default {
  name: "typo",
  input: { names: "array" },
  model: "mock",
  states: { fan },
  flow: pipe(fan, done),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err = %v, want forEach unknown-key error for onItemFail", err)
	}
}
