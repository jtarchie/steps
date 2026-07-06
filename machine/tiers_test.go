package machine

import (
	"strings"
	"testing"
)

// tierMachine wires a single agent state that selects a tier, plus a machine
// default, so the precedence cascade (state > tier > defaults > engine) is
// observable on one resolved AgentSpec.
func tierMachine(models, stateBody string) string {
	return `
const work = ` + stateBody + `;
export default {
  name: "tiers",
  models: ` + models + `,
  states: { work },
  flow: pipe(work, done),
};`
}

func TestTierKnobsFillState(t *testing.T) {
	m, err := Parse([]byte(tierMachine(
		`{ scout: { model: "mock", reasoning: "low", maxOutputTokens: 400, memo: true } }`,
		`{ model: "scout", prompt: "hi" }`,
	)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := m.State("work")
	a := s.Agent
	if a.Model.Static != "mock" {
		t.Errorf("model = %v, want resolved ref mock", a.Model.Static)
	}
	if a.Reasoning != "low" {
		t.Errorf("reasoning = %q, want low from tier", a.Reasoning)
	}
	if a.MaxOutputTokens != 400 {
		t.Errorf("maxOutputTokens = %d, want 400 from tier", a.MaxOutputTokens)
	}
	if !s.Memo {
		t.Errorf("memo = false, want true from tier")
	}
}

func TestTierStateExplicitWins(t *testing.T) {
	m, err := Parse([]byte(tierMachine(
		`{ scout: { model: "mock", reasoning: "low", maxOutputTokens: 400, memo: true } }`,
		`{ model: "scout", prompt: "hi", reasoning: "high", maxOutputTokens: 9000, memo: false }`,
	)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := m.State("work")
	if s.Agent.Reasoning != "high" {
		t.Errorf("reasoning = %q, want high (state wins)", s.Agent.Reasoning)
	}
	if s.Agent.MaxOutputTokens != 9000 {
		t.Errorf("maxOutputTokens = %d, want 9000 (state wins)", s.Agent.MaxOutputTokens)
	}
	if s.Memo {
		t.Errorf("memo = true, want false (explicit memo: false beats tier)")
	}
}

func TestTierPlainStringStillWorks(t *testing.T) {
	m, err := Parse([]byte(tierMachine(
		`{ scout: "mock" }`,
		`{ model: "scout", prompt: "hi" }`,
	)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := m.State("work").Agent.Model.Static; got != "mock" {
		t.Errorf("model = %v, want mock", got)
	}
}

func TestTierViaMachineDefault(t *testing.T) {
	// A state that names no model inherits the default tier's knobs.
	src := `
const work = { prompt: "hi" };
export default {
  name: "tiers",
  model: "senior",
  models: { senior: { model: "mock", reasoning: "high", maxOutputTokens: 8192 } },
  states: { work },
  flow: pipe(work, done),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := m.State("work").Agent
	if a.Model.Static != "mock" || a.Reasoning != "high" || a.MaxOutputTokens != 8192 {
		t.Errorf("default-tier state = {model:%v reasoning:%q max:%d}, want mock/high/8192",
			a.Model.Static, a.Reasoning, a.MaxOutputTokens)
	}
}

func TestTierErrors(t *testing.T) {
	cases := map[string]string{
		"missing model":   `{ scout: { reasoning: "low" } }`,
		"unknown key":     `{ scout: { model: "mock", think: "hard" } }`,
		"bad reasoning":   `{ scout: { model: "mock", reasoning: "ultra" } }`,
		"unqualified ref": `{ scout: { model: "gemma" } }`,
	}
	for name, models := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tierMachine(models, `{ model: "scout", prompt: "hi" }`)))
			if err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

func TestTierMemoOnlyFillsUndeclared(t *testing.T) {
	// A tier without memo leaves the state's memo untouched.
	m, err := Parse([]byte(tierMachine(
		`{ scout: { model: "mock", reasoning: "low" } }`,
		`{ model: "scout", prompt: "hi" }`,
	)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.State("work").Memo {
		t.Errorf("memo = true, want false (tier declared none)")
	}
	// Sanity: the error message names the tier when the ref is bad.
	_, err = Parse([]byte(tierMachine(`{ scout: { model: "nope" } }`, `{ model: "scout", prompt: "hi" }`)))
	if err == nil || !strings.Contains(err.Error(), "scout") {
		t.Errorf("error = %v, want it to name the scout tier", err)
	}
}
