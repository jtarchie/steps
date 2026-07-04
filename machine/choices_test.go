package machine

import (
	"strings"
	"testing"
)

// wrapGate builds a machine with one gate branching to publish/fail so the
// resume-event alphabet is {approved, rejected, timeout}.
func wrapGate(gate string) string {
	return `
const work = { prompt: "produce" };
const gate = ` + gate + `;
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "choices",
  model: "mock",
  states: { work, gate, ship },
  flow: pipe(
    work,
    branch(gate, { approved: ship, rejected: fail, timeout: fail }),
  ),
};`
}

func TestChoicesSingle(t *testing.T) {
	m, err := Parse([]byte(wrapGate(`{
  human: "Approve?",
  choices: { approved: "Ship it", rejected: "Abort" },
  timeout: "1h",
}`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := m.State("gate").Human.Choices
	if c == nil || c.Kind != "single" {
		t.Fatalf("choices = %+v, want single", c)
	}
	if len(c.Options) != 2 ||
		c.Options[0] != (ChoiceOption{Event: "approved", Label: "Ship it"}) ||
		c.Options[1] != (ChoiceOption{Event: "rejected", Label: "Abort"}) {
		t.Errorf("options = %+v, want declared pairs in order", c.Options)
	}
}

func TestChoicesFreeFormGateStillValid(t *testing.T) {
	m, err := Parse([]byte(wrapGate(`{ human: "Approve?", timeout: "1h" }`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.State("gate").Human.Choices != nil {
		t.Errorf("free-form gate should have nil Choices")
	}
}

func TestChoicesMultiStatic(t *testing.T) {
	m, err := Parse([]byte(wrapGate(`{
  human: "Pick modules",
  choices: { multi: ["auth", "billing"], event: "approved", min: 1, max: 2 },
  timeout: "1h",
}`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := m.State("gate").Human.Choices
	if c == nil || c.Kind != "multi" || c.Event != "approved" || c.Min != 1 || c.Max != 2 {
		t.Fatalf("choices = %+v, want multi/approved/1/2", c)
	}
	items, _ := c.Dynamic.Static.([]any)
	if len(items) != 2 || items[0] != "auth" {
		t.Errorf("static options = %+v", items)
	}
}

func TestChoicesMultiFnAndEventDefault(t *testing.T) {
	// One event edge -> event: defaults to it.
	src := `
const work = { prompt: "scan", output: { modules: { type: "array", items: "string" } } };
const gate = {
  human: "Pick modules",
  choices: { multi: ({ work }) => work.modules },
};
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "choices",
  model: "mock",
  states: { work, gate, ship },
  flow: pipe(work, branch(gate, { selected: ship })),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := m.State("gate").Human.Choices
	if c.Event != "selected" {
		t.Errorf("event = %q, want defaulted to the single edge %q", c.Event, "selected")
	}
	if !c.Dynamic.IsFn() {
		t.Errorf("multi should be a function")
	}
}

func TestChoicesErrors(t *testing.T) {
	cases := []struct {
		name, gate, want string
	}{
		{
			"unknown option event",
			`{ human: "?", choices: { ship: "Ship it" }, timeout: "1h" }`,
			`choice "ship" is not a resume event`,
		},
		{
			"multi needs event with several edges",
			`{ human: "?", choices: { multi: ["a"] }, timeout: "1h" }`,
			"multi choices need event:",
		},
		{
			"multi unknown event",
			`{ human: "?", choices: { multi: ["a"], event: "nope" }, timeout: "1h" }`,
			`event "nope" is not a resume event`,
		},
		{
			"max below min",
			`{ human: "?", choices: { multi: ["a"], event: "approved", min: 2, max: 1 }, timeout: "1h" }`,
			"max (1) must be >= min (2)",
		},
		{
			"non-string label",
			`{ human: "?", choices: { approved: 7 }, timeout: "1h" }`,
			"label must be a string",
		},
		{
			"unknown multi key",
			`{ human: "?", choices: { multi: ["a"], event: "approved", oops: 1 }, timeout: "1h" }`,
			`unknown key "oops" for multi choices`,
		},
		{
			"empty choices",
			`{ human: "?", choices: {}, timeout: "1h" }`,
			"at least one option",
		},
		{
			"multi not an array",
			`{ human: "?", choices: { multi: "a", event: "approved" }, timeout: "1h" }`,
			"must be an array of strings or a function",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(wrapGate(tc.gate)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestChoicesScopeChecking: a multi: function reading an unknown scope key
// fails before any token is spent — destructured params at parse time (the
// contract check), dynamic access at dry-run time. The machine declares
// input: — that is what buys strict scope checking.
func TestChoicesScopeChecking(t *testing.T) {
	wrap := func(multi string) string {
		return `
const work = { prompt: "scan", output: { modules: { type: "array", items: "string" } } };
const gate = {
  human: "Pick",
  choices: { multi: ` + multi + ` },
};
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "choices",
  model: "mock",
  input: { spec: "string" },
  states: { work, gate, ship },
  flow: pipe(work, branch(gate, { selected: ship })),
};`
	}

	t.Run("destructured typo fails the parse", func(t *testing.T) {
		_, err := Parse([]byte(wrap(`({ nope }) => nope`)))
		if err == nil || !strings.Contains(err.Error(), "choices.multi") || !strings.Contains(err.Error(), "nope") {
			t.Errorf("err = %v, want a choices.multi contract failure naming nope", err)
		}
	})

	t.Run("dynamic access fails the load-time dry-run", func(t *testing.T) {
		_, err := Parse([]byte(wrap(`(s) => s.nope`)))
		if err == nil || !strings.Contains(err.Error(), "human.choices.multi") || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("err = %v, want a human.choices.multi dry-run failure on the unknown field", err)
		}
	})
}
