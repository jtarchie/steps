// Package machine defines the workflow model: states, transitions, guards,
// retry policies, and limits. A Machine is loaded from YAML (or built in Go),
// expanded with defaults, and validated before it can run.
package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/expr-lang/expr/vm"
	"github.com/google/jsonschema-go/jsonschema"
)

// Machine is an immutable, validated workflow definition.
type Machine struct {
	Version     int
	Name        string
	Description string
	Input       map[string]InputSpec
	Defaults    Defaults
	Limits      Limits
	Initial     string
	States      []*State // document order preserved

	RawYAML []byte // exact bytes the machine was loaded from ("" when built in Go)
	Hash    string // sha256 of RawYAML

	index map[string]*State
}

// State returns the named state, or nil.
func (m *Machine) State(name string) *State { return m.index[name] }

func (m *Machine) buildIndex() {
	m.index = make(map[string]*State, len(m.States))
	for _, s := range m.States {
		m.index[s.Name] = s
	}
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// InputSpec declares a required/optional run input.
type InputSpec struct {
	Type     string
	Required bool
}

// Defaults is the machine-level `defaults:` block.
type Defaults struct {
	Agent AgentDefaults
	Retry []RetryPolicy // when set, replaces the engine default retry policy
}

// AgentDefaults cascade into every agent state that leaves the field unset.
type AgentDefaults struct {
	Model            string
	Temperature      *float64
	MaxTurns         int
	MaxOutputTokens  int
	StructuredOutput string // prompt (default) | native
	Reasoning        string // low | medium | high ("" = provider default)
}

// Limits are run-level guardrails. Zero values mean "engine default" for
// MaxTransitions/Timeout and "off" for MaxCost/MaxTokens.
type Limits struct {
	MaxTransitions int
	MaxCost        float64
	MaxTokens      int
	Timeout        time.Duration
}

const (
	DefaultMaxTransitions  = 50
	DefaultTimeout         = 15 * time.Minute
	DefaultMaxTurns        = 10
	DefaultMaxOutputTokens = 2048 // per model call — no state may generate unboundedly
)

// State is one node of the machine: exactly one handler, contracts, policies.
type State struct {
	Name     string
	Agent    *AgentSpec
	Action   *ActionSpec
	Human    *HumanSpec
	Terminal bool
	Status   string // "" (success) or "failed" — terminal states only

	Input       map[string]string // templated inputs (action args / agent user message)
	Output      OutputSpec
	Retry       []RetryPolicy
	Catch       []CatchClause
	Transitions []Transition
}

// HandlerKind reports which handler the state runs.
func (s *State) HandlerKind() string {
	switch {
	case s.Terminal:
		return "terminal"
	case s.Agent != nil:
		return "agent"
	case s.Action != nil:
		return "action"
	case s.Human != nil:
		return "human"
	}
	return "invalid"
}

// AgentSpec configures an LLM agent-loop handler.
type AgentSpec struct {
	Model       string
	System      string
	Prompt      string
	Tools       []ToolRef
	MaxTurns    int
	Temperature *float64
	Adopt       string // "", "self", or a predecessor state name
	// AdoptLastTurns trims the adopted transcript to its last N messages
	// (0 = all). Token hygiene for long revision loops.
	AdoptLastTurns int
	// MaxOutputTokens caps each model call. A state may never generate
	// unboundedly — grammar-degenerate or runaway completions become a
	// bounded failure instead of a hang.
	MaxOutputTokens int
	// StructuredOutput selects how the output contract is enforced:
	// "prompt" (default, portable) embeds the schema in the instruction;
	// "native" additionally constrains the decoder on providers that
	// support it (OpenAI-compatible response_format json_schema). Native
	// is a token win on well-supported backends, but grammar-constrained
	// sampling degenerates on some local model/backend combos — opt in.
	StructuredOutput string
	// Reasoning caps the model's thinking effort (low | medium | high;
	// "" = provider default). Reasoning tokens are billed output — a
	// drafting micro-agent rarely needs deep thought, a judge might.
	Reasoning string
	History   *HistorySpec
	ToolChoice  string // auto (default) | required | one_of — one_of not yet implemented
}

// ToolRef attaches a registered tool to an agent state, optionally guarded.
type ToolRef struct {
	Name     string
	MaxCalls int    // 0 = unlimited
	When     string // Expr guard evaluated at call time (env includes args)
	OnReject string // feedback (default) | fail
	Require  string // another tool that must have been called first

	Guard *vm.Program // compiled from When at load time
}

// HistorySpec injects a rendered journal projection of a prior state.
type HistorySpec struct {
	From      string
	Include   []string // messages | tool_calls (default both)
	LastTurns int      // 0 = all
	As        string   // template variable name (default "trace")
}

// ActionSpec names a registered Go function; the rendered state Input is its args.
type ActionSpec struct {
	Name string
}

// HumanSpec parks the run until a human resumes it.
type HumanSpec struct {
	Prompt    string
	Timeout   time.Duration // 0 = no timeout
	OnTimeout string        // state routed to when the gate expires
}

// OutputSpec is the state's output contract.
type OutputSpec struct {
	Schema map[string]any // property name -> JSON-schema fragment (or scalar type shorthand)
	Events []string

	Compiled *jsonschema.Resolved // resolved at load time; nil when Schema is empty
}

// DefaultOutput reports whether this is the implicit {text: string} contract.
func (o OutputSpec) DefaultOutput() bool {
	if len(o.Schema) != 1 {
		return false
	}
	v, ok := o.Schema["text"]
	return ok && v == "string"
}

// Transition routes out of a state. On matches the agent-declared event;
// When is an Expr guard. Both optional; both must hold when present.
type Transition struct {
	On   string
	When string
	To   string

	Guard *vm.Program // compiled from When at load time
}

// Fallback reports whether the transition matches unconditionally.
func (t Transition) Fallback() bool { return t.On == "" && t.When == "" }

// RetryPolicy retries handler failures whose class is in Match.
type RetryPolicy struct {
	Match       []string
	MaxAttempts int
	Backoff     Backoff
}

// Matches reports whether the policy covers the error class.
func (r RetryPolicy) Matches(class string) bool {
	for _, m := range r.Match {
		if m == class || m == "*" {
			return true
		}
	}
	return false
}

// Backoff is exponential with optional jitter.
type Backoff struct {
	Initial time.Duration
	Factor  float64
	Jitter  bool
	Cap     time.Duration
}

// Delay computes the backoff before the given retry (attempt starts at 1).
func (b Backoff) Delay(attempt int) time.Duration {
	d := float64(b.Initial)
	for i := 1; i < attempt; i++ {
		d *= b.Factor
	}
	if b.Cap > 0 && time.Duration(d) > b.Cap {
		d = float64(b.Cap)
	}
	return time.Duration(d)
}

// CatchClause routes unrecoverable failures to a state.
type CatchClause struct {
	Match []string
	To    string
}

// Matches reports whether the clause covers the error class.
func (c CatchClause) Matches(class string) bool {
	for _, m := range c.Match {
		if m == class || m == "*" {
			return true
		}
	}
	return false
}

// Error classes used by retry/catch matching. Handlers and the engine
// classify every failure into one of these.
const (
	ClassRateLimited     = "rate_limited"
	ClassProviderError   = "provider_error"
	ClassActionError     = "action_error"
	ClassTimeout         = "timeout"
	ClassSchemaViolation = "schema_violation"
	ClassGuardRejected   = "guard_rejected"
	ClassRetriesExhausted = "retries_exhausted"
	ClassBudgetExceeded  = "budget_exceeded"
	ClassMaxTransitions  = "max_transitions"
	ClassRunTimeout      = "run_timeout"
	ClassAdoptMissing    = "adopt_missing"
)
