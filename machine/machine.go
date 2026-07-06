// Package machine defines the workflow model: states, transitions, guards,
// retry policies, and limits. A Machine is a JavaScript file (evaluated by
// goja) exporting a data structure; any computed value is a plain JS
// function. Structure is data, logic is one honest language — nothing is
// smuggled into strings.
package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Machine is an immutable, validated workflow definition.
type Machine struct {
	Version     int
	Name        string
	Description string
	Input       map[string]InputSpec
	// Models are human-named tiers (scout, senior) for provider refs — states
	// read semantically, and swapping backends is one edit. A tier bundles the
	// ref with the per-role knobs (reasoning, maxOutputTokens, memo) so "cheap
	// scout vs expensive senior" is declared once, not restated per state.
	Models   map[string]ModelSpec
	Defaults Defaults
	Limits   Limits
	Initial  string
	States   []*State // declaration order preserved

	// Webhook declares how an inbound HTTP payload becomes a run of this
	// machine (trigger-only; served by `steps serve --hook`). Map is a
	// function of one flat scope {body, headers, query, ...hook inputs}
	// returning run inputs.
	Webhook *WebhookSpec

	Source []byte            // exact JS bytes the machine was loaded from
	Assets map[string]string // include()d files, pinned with the source
	Hash   string            // sha256 over Source + Assets

	index map[string]*State
	rt    *jsRT // shared runtime for every Dyn in this machine
}

// State returns the named state, or nil.
func (m *Machine) State(name string) *State { return m.index[name] }

func (m *Machine) buildIndex() {
	m.index = make(map[string]*State, len(m.States))
	for _, s := range m.States {
		m.index[s.Name] = s
	}
}

func hashMachine(src []byte, assets map[string]string) string {
	h := sha256.New()
	h.Write(src)
	keys := make([]string, 0, len(assets))
	for k := range assets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(assets[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// InputSpec declares a required/optional run input.
type InputSpec struct {
	Type     string
	Required bool
}

// ModelSpec is one entry of the machine's models: block — a named tier. Ref is
// the provider-namespaced model (or an engine alias like "mock"); the optional
// knobs cascade into any agent state that selects this tier and leaves the
// field unset (precedence: state-explicit > tier > machine defaults > engine
// default). The plain-string models: form (models: { x: "anthropic/..." })
// parses to a ModelSpec with only Ref set.
type ModelSpec struct {
	Ref             string
	Reasoning       string // low | medium | high ("" = leave to the cascade)
	MaxOutputTokens int    // 0 = leave to the cascade
	Memo            *bool  // nil = leave to the state/cascade
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
	MaxInputTokens   *int   // nil = unset (0 means off); distill states are exempt from this cascade
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
	DefaultMaxTransitions   = 50
	DefaultTimeout          = 15 * time.Minute
	DefaultMaxTurns         = 10   // states with tools: room for a tool-use loop
	DefaultMaxTurnsToolless = 2    // one model call per turn; 2 is headroom for a semantic retry
	DefaultMaxOutputTokens  = 2048 // per model call — no state may generate unboundedly
	// DefaultMaxInputTokens is the input mirror of maxOutputTokens: no state
	// may read unboundedly either. Sized for the practical local-model floor
	// (8k in + 2k out fits a 16k window with headroom); every overflow is a
	// zero-token failure that names the largest inputs. maxInputTokens: 0
	// opts a state (or the machine) out.
	DefaultMaxInputTokens = 8192
)

// State is one node of the machine: exactly one handler, contracts, policies.
type State struct {
	Name     string
	Agent    *AgentSpec
	Action   *ActionSpec
	Human    *HumanSpec
	Terminal bool
	Status   string // "" (success) or "failed" — terminal states only

	// Input: action args / agent user message. A static object (whose values
	// may individually be functions) or one function returning the whole map.
	Input    Dyn
	ForEach  *ForEachSpec  // fan the handler out over a list from ctx
	Parallel *ParallelSpec // fork into concurrent branches, join at a barrier
	// Memo replays the journaled output when the rendered input (model +
	// system + prompt) is byte-identical to a previous execution — re-runs
	// only re-pay for what changed. Agent states only: actions have side
	// effects that must not be skipped.
	Memo bool
	// memoDeclared records whether the state's source set memo: explicitly, so a
	// model tier's memo knob only fills a state that stayed silent (an explicit
	// memo: false always wins over the tier).
	memoDeclared bool
	// Gate marks a state synthesized by the gate() combinator (a human
	// escalation whose branch tail was generated). Named `gate#<name>` — like
	// distill's `owner#key`, the `#` keeps it collision-free and
	// non-destructurable.
	Gate bool
	// Verdict is the state's own acceptance test, declared once: a function of
	// the guard scope ({output, event, ...}) returning a boolean. loop() uses it
	// as the accept edge when accept: is omitted, so the criterion, the output
	// schema, and the routing stop being restated in three places.
	Verdict Dyn
	Output  OutputSpec
	// Distill entries replace (or derive from) large scope values with
	// model-extracted slices before this state runs. Each entry lowers to an
	// implicit agent state (`name#key`) in ApplyDefaults — see distill.go.
	Distill []DistillEntry
	// DistillOf/DistillKey mark a lowered implicit distill state: the
	// consumer state it serves and the scope key it produces.
	DistillOf   string
	DistillKey  string
	Retry       []RetryPolicy
	Catch       []CatchClause
	Transitions []Transition
}

// ForEachSpec runs the state's handler once per item of a list evaluated
// from ctx. Each item gets a fresh, hermetic context (agent items are
// separate conversations — N small windows instead of one big one). The
// state's output schema describes the PER-ITEM shape; the state's ctx entry
// becomes {items: [...], count: n}. Sequential in v1; items share the
// state's retry policy.
type ForEachSpec struct {
	Over Dyn    // function of scope returning the list
	As   string // scope name for the current item (default "item")
	// Concurrency bounds parallel items (default 1; mock runs force 1 so
	// scripted queues stay deterministic).
	Concurrency int
	// OnItemFailure: fail (default — one bad item fails the state) or skip
	// (drop the item; aggregate output reports skipped/failures for guards).
	OnItemFailure string
	// Carry pairs each output with its source item: aggregate items entries
	// become {item, output, index} instead of the bare output. index is the
	// position in the original over list, so the pairing stays correct even when
	// onItemFailure: "skip" drops entries — the misalignment that hand-written
	// items[i] zips silently produce.
	Carry bool
}

// ParallelSpec forks the run into concurrent branches — each a hermetic
// sub-run of states from the SAME machine — and joins them at a barrier. It is
// the heterogeneous generalization of ForEach: instead of one handler over N
// homogeneous items, N distinct sub-flows run at once, each with its own
// hermetic context. The state's ctx entry becomes a label-keyed object
// {label: branchOutput, ...} (plus _failures under onBranchFailure: "collect");
// the join is the fork state's successor, which reads the aggregate from the
// flat scope. A fork is one node = one journal entry = one retry/catch owner.
type ParallelSpec struct {
	Branches        []Branch // label + entry state, declaration (object-key) order
	Concurrency     int      // bounds simultaneously-running branches (default 1)
	OnBranchFailure string   // "fail" (default — first failed branch fails the fork) | "collect"
}

// Branch is one arm of a parallel fork: a label (the aggregate key the join
// reads) and the entry state of its sub-flow.
type Branch struct {
	Label string
	Entry string
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
	case s.Parallel != nil:
		return "parallel"
	}
	return "invalid"
}

// AgentSpec configures an LLM agent-loop handler.
type AgentSpec struct {
	// Model: a static alias/provider ref, or a function of scope returning
	// one — per-execution routing (e.g. by a foreach item's risk).
	Model       Dyn
	System      Dyn // static string or function of scope
	Prompt      Dyn // static string or function of scope
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
	// MaxInputTokens caps the rendered input (system + user message,
	// chars/4 estimate). nil cascades defaults -> DefaultMaxInputTokens; an
	// author's 0 means off. Over-budget classifies budget_exceeded — never
	// retried, routable by catch:, attributed to the largest inputs. The
	// enforcement half of the context thesis; distill is the fix at the
	// callsite, and implicit distill states are exempt (the distiller is the
	// one place the big payload is supposed to appear).
	MaxInputTokens *int
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
	Reasoning  string
	History    *HistorySpec
	ToolChoice string // auto (default) | required | one_of — one_of not yet implemented

	// Evidence declares the labeled blocks appended to the prompt: prompt: is
	// the hand-written instruction, evidence: is the mechanical plumbing that
	// re-injects upstream values (ARTICLE:\n${article}) — declared as data
	// instead of hand-templated. Each entry renders as LABEL:\nvalue in
	// declaration order; a falsy value (undefined/null/false/"") omits its
	// block, which is how conditional revision feedback disappears on the
	// first visit. Lowered at parse into a composed Prompt (Dyn.Native);
	// instruction keeps the original for dry-run/contract checks.
	Evidence    []EvidenceEntry
	instruction Dyn
}

// EvidenceEntry is one labeled block of an agent's evidence: declaration.
// A zero Value means "the scope key named by Key" (the `article: true` form).
type EvidenceEntry struct {
	Key   string
	Value Dyn
}

// ToolRef attaches a registered tool to an agent state, optionally guarded.
type ToolRef struct {
	Name     string
	MaxCalls int // 0 = unlimited
	// When guards each call: a function of scope (which includes the
	// model-authored args) returning bool.
	When     Dyn
	OnReject string // feedback (default) | fail
	Require  string // another tool that must have been called first
	// Args pins machine-authored args merged over the model's at execution —
	// repo roots, IDs, credentials-by-ref. A static object or a function of
	// scope returning one. The model never sees or overrides them.
	Args Dyn
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

// WebhookSpec maps an inbound HTTP payload to run inputs (trigger-only).
type WebhookSpec struct {
	Path string // URL slug under /hooks/; defaults to the machine name
	Map  Dyn    // function of {body, headers, query, ...hook inputs} -> run inputs
	// MaxInFlight bounds concurrent runs of this hook; 0 = DefaultHookMaxInFlight.
	// MaxQueued bounds durably-queued runs awaiting a slot; 0 = DefaultHookMaxQueued.
	// Overflowing MaxQueued rejects the POST with 429.
	MaxInFlight int
	MaxQueued   int
}

const (
	// DefaultHookMaxInFlight serializes a hook unless it opts up — one run at a
	// time is the safe default for side-effecting workflows.
	DefaultHookMaxInFlight = 1
	// DefaultHookMaxQueued bounds the durable queue so 429 backpressure fires;
	// an unbounded queue is the footgun this feature removes.
	DefaultHookMaxQueued = 100
)

// HumanSpec parks the run until a human resumes it.
type HumanSpec struct {
	Prompt    Dyn           // static string or function of scope
	Timeout   time.Duration // 0 = no timeout
	OnTimeout string        // state routed to when the gate expires
	Choices   *ChoiceSpec   // nil = free-form-only gate
}

// ChoiceSpec declares how a gate's answer is collected. Two forms:
// single (confirm is a two-option single) maps each option to one of the
// gate's resume events; multi collects a subset of options, emits ONE event,
// and carries the selection in the gate's output as `selected`. Every gate
// answer may additionally carry a free-form `note` string.
type ChoiceSpec struct {
	Kind    string         // "single" | "multi"
	Options []ChoiceOption // single: event/label pairs, declaration order
	Dynamic Dyn            // multi: static []string or function of scope
	Event   string         // multi: the one resume event emitted
	Min     int            // multi: minimum selections (0 = none)
	Max     int            // multi: maximum selections (0 = unbounded)
}

// ChoiceOption is one selectable answer on a single-choice gate.
type ChoiceOption struct {
	Event string // the resume event this option fires
	Label string // human-readable description
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
// When is a guard function of scope. Both optional; both must hold when
// present.
type Transition struct {
	On   string
	When Dyn
	To   string
}

// Fallback reports whether the transition matches unconditionally.
func (t Transition) Fallback() bool { return t.On == "" && t.When.IsZero() }

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
	ClassRateLimited      = "rate_limited"
	ClassProviderError    = "provider_error"
	ClassActionError      = "action_error"
	ClassTimeout          = "timeout"
	ClassSchemaViolation  = "schema_violation"
	ClassGuardRejected    = "guard_rejected"
	ClassRetriesExhausted = "retries_exhausted"
	ClassBudgetExceeded   = "budget_exceeded"
	ClassMaxTransitions   = "max_transitions"
	ClassRunTimeout       = "run_timeout"
	ClassAdoptMissing     = "adopt_missing"
)
