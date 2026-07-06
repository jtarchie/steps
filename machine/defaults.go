package machine

import "time"

// DefaultRetryPolicies is the engine default applied to every state that does
// not declare its own (and whose machine has no defaults.retry):
// transient 3x exponential (1s x2, jitter, 30s cap); semantic 2x with feedback.
func DefaultRetryPolicies() []RetryPolicy {
	return []RetryPolicy{
		{
			Match:       []string{ClassRateLimited, ClassProviderError, ClassActionError, ClassTimeout},
			MaxAttempts: 3,
			Backoff:     Backoff{Initial: time.Second, Factor: 2.0, Jitter: true, Cap: 30 * time.Second},
		},
		{
			Match:       []string{ClassSchemaViolation, ClassGuardRejected},
			MaxAttempts: 2,
		},
	}
}

// ApplyDefaults expands convention-over-configuration before validation, so
// the machine that runs is always fully explicit (`steps validate --print`).
func ApplyDefaults(m *Machine) {
	// initial: first state in document order.
	if m.Initial == "" && len(m.States) > 0 {
		m.Initial = m.States[0].Name
	}

	applyLinearFlowDefaults(m)
	ensureImplicitTerminals(m)
	m.buildIndex()

	// distill: lowers to implicit `name#key` states — after every edge exists
	// (flow or linear defaults), before the cascade below defaults them like
	// any other agent state.
	lowerDistill(m)

	// Limits.
	if m.Limits.MaxTransitions == 0 {
		m.Limits.MaxTransitions = DefaultMaxTransitions
	}
	if m.Limits.Timeout == 0 {
		m.Limits.Timeout = DefaultTimeout
	}

	// Model tiers resolve per state (applyAgentDefaults reads the alias name to
	// pull its knobs before the machine-default cascade). Capture the defaults
	// block's own tier first, then resolve its ref after the loop so --print
	// shows a concrete ref there too.
	defaultTier, defaultIsTier := m.Models[m.Defaults.Agent.Model]

	for _, s := range m.States {
		if s.Terminal {
			continue
		}
		if a := s.Agent; a != nil {
			applyAgentDefaults(m, s, a)
		}
		if f := s.ForEach; f != nil {
			applyForEachDefaults(f)
		}
		if p := s.Parallel; p != nil {
			applyParallelDefaults(p)
		}
		applyRetryDefaults(m, s)
	}

	if defaultIsTier {
		m.Defaults.Agent.Model = defaultTier.Ref
	}
}

// effectiveTier returns the model tier a state resolves to — its own static
// model: alias, or the machine default when the state names none — if that
// alias is a tier declared in models:.
func effectiveTier(m *Machine, a *AgentSpec) (ModelSpec, bool) {
	alias := ""
	if ref, ok := a.Model.Static.(string); ok {
		alias = ref
	} else if a.Model.IsZero() {
		alias = m.Defaults.Agent.Model
	}
	spec, ok := m.Models[alias]
	return spec, ok
}

// applyModelTier fills the state's per-role knobs from its tier, but only where
// the state stayed silent — state-explicit always wins over the tier.
func applyModelTier(m *Machine, s *State, a *AgentSpec) {
	spec, ok := effectiveTier(m, a)
	if !ok {
		return
	}
	if a.Reasoning == "" {
		a.Reasoning = spec.Reasoning
	}
	if a.MaxOutputTokens == 0 {
		a.MaxOutputTokens = spec.MaxOutputTokens
	}
	if spec.Memo != nil && !s.memoDeclared {
		s.Memo = *spec.Memo
	}
}

// applyLinearFlowDefaults: a state with no transitions flows to the next
// state in document order; the last state flows to done. Computed over the
// user-declared list, before implicit terminals are appended.
func applyLinearFlowDefaults(m *Machine) {
	declared := len(m.States)
	for i := range declared {
		s := m.States[i]
		if s.Terminal || len(s.Transitions) > 0 {
			continue
		}
		next := "done"
		if i+1 < declared {
			next = m.States[i+1].Name
		}
		s.Transitions = []Transition{{To: next}}
	}
}

func ensureImplicitTerminals(m *Machine) {
	if m.State("done") == nil {
		m.States = append(m.States, &State{Name: "done", Terminal: true})
	}
	if m.State("failed") == nil {
		m.States = append(m.States, &State{Name: "failed", Terminal: true, Status: "failed"})
	}
}

// applyAgentDefaults cascades an agent state's knobs: state -> defaults.agent
// -> engine default.
func applyAgentDefaults(m *Machine, s *State, a *AgentSpec) {
	applyModelTier(m, s, a)
	applyAgentModelDefault(m, a)
	applyAgentTemperatureDefault(m, a)
	applyAgentTurnsDefault(m, a)
	applyAgentTokenDefaults(m, s, a)
	if a.StructuredOutput == "" {
		a.StructuredOutput = m.Defaults.Agent.StructuredOutput
	}
	if a.StructuredOutput == "" {
		a.StructuredOutput = "prompt"
	}
	if a.Reasoning == "" {
		a.Reasoning = m.Defaults.Agent.Reasoning
	}
	if a.ToolChoice == "" {
		a.ToolChoice = "auto"
	}
	for i := range a.Tools {
		if a.Tools[i].OnReject == "" {
			a.Tools[i].OnReject = "feedback"
		}
	}
	if h := a.History; h != nil {
		if len(h.Include) == 0 {
			h.Include = []string{"messages", "tool_calls"}
		}
		if h.As == "" {
			h.As = "trace"
		}
	}
	// Output contract default: {text: string}, no events.
	if len(s.Output.Schema) == 0 {
		s.Output.Schema = map[string]any{"text": "string"}
	}
}

func applyAgentModelDefault(m *Machine, a *AgentSpec) {
	if a.Model.IsZero() && m.Defaults.Agent.Model != "" {
		a.Model = Dyn{Static: m.Defaults.Agent.Model}
	}
	// Static aliases resolve at load; function results resolve at runtime
	// (the engine re-checks against Models).
	if ref, ok := a.Model.Static.(string); ok {
		if resolved, isAlias := m.Models[ref]; isAlias {
			a.Model = Dyn{Static: resolved.Ref}
		}
	}
}

func applyAgentTemperatureDefault(m *Machine, a *AgentSpec) {
	if a.Temperature == nil {
		a.Temperature = m.Defaults.Agent.Temperature
	}
	if a.Temperature == nil {
		zero := 0.0
		a.Temperature = &zero
	}
}

func applyAgentTurnsDefault(m *Machine, a *AgentSpec) {
	if a.MaxTurns != 0 {
		return
	}
	a.MaxTurns = m.Defaults.Agent.MaxTurns
	if a.MaxTurns != 0 {
		return
	}
	// Tool-less states make one model call per turn — 2 is headroom; only a
	// tool-use loop needs room to iterate.
	if len(a.Tools) > 0 {
		a.MaxTurns = DefaultMaxTurns
	} else {
		a.MaxTurns = DefaultMaxTurnsToolless
	}
}

func applyAgentTokenDefaults(m *Machine, s *State, a *AgentSpec) {
	if a.MaxOutputTokens == 0 {
		a.MaxOutputTokens = m.Defaults.Agent.MaxOutputTokens
	}
	if a.MaxOutputTokens == 0 {
		a.MaxOutputTokens = DefaultMaxOutputTokens
	}
	// maxInputTokens: nil cascades to the engine default; an author's 0 means
	// off. Never cascades onto implicit distill states — the distiller is the
	// one place the big payload is supposed to appear.
	if a.MaxInputTokens == nil && !s.IsDistill() {
		a.MaxInputTokens = m.Defaults.Agent.MaxInputTokens
	}
	if a.MaxInputTokens == nil && !s.IsDistill() {
		maxInput := DefaultMaxInputTokens
		a.MaxInputTokens = &maxInput
	}
}

func applyForEachDefaults(f *ForEachSpec) {
	if f.As == "" {
		f.As = "item"
	}
	if f.Concurrency == 0 {
		f.Concurrency = 1
	}
	if f.OnItemFailure == "" {
		f.OnItemFailure = "fail"
	}
}

func applyParallelDefaults(p *ParallelSpec) {
	if p.Concurrency == 0 {
		p.Concurrency = 1
	}
	if p.OnBranchFailure == "" {
		p.OnBranchFailure = "fail"
	}
}

// applyRetryDefaults: nil means unset -> machine defaults.retry -> engine
// default. An explicit empty slice (retry: none) stays empty.
func applyRetryDefaults(m *Machine, s *State) {
	if s.Retry != nil {
		return
	}
	if m.Defaults.Retry != nil {
		s.Retry = m.Defaults.Retry
	} else {
		s.Retry = DefaultRetryPolicies()
	}
}
