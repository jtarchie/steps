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

	// Linear flow: a state with no transitions flows to the next state in
	// document order; the last state flows to done. Computed over the
	// user-declared list, before implicit terminals are appended.
	declared := len(m.States)
	for i := 0; i < declared; i++ {
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

	// Implicit terminals.
	if m.State("done") == nil {
		m.States = append(m.States, &State{Name: "done", Terminal: true})
	}
	if m.State("failed") == nil {
		m.States = append(m.States, &State{Name: "failed", Terminal: true, Status: "failed"})
	}
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

	// Model aliases resolve everywhere a ref appears, including the
	// defaults block itself.
	if ref, ok := m.Models[m.Defaults.Agent.Model]; ok {
		m.Defaults.Agent.Model = ref
	}

	zero := 0.0
	for _, s := range m.States {
		if s.Terminal {
			continue
		}

		// Agent cascade: state -> defaults.agent -> engine default.
		if a := s.Agent; a != nil {
			if a.Model.IsZero() && m.Defaults.Agent.Model != "" {
				a.Model = Dyn{Static: m.Defaults.Agent.Model}
			}
			// Static aliases resolve at load; function results resolve at
			// runtime (the engine re-checks against Models).
			if ref, ok := a.Model.Static.(string); ok {
				if resolved, isAlias := m.Models[ref]; isAlias {
					a.Model = Dyn{Static: resolved}
				}
			}
			if a.Temperature == nil {
				a.Temperature = m.Defaults.Agent.Temperature
			}
			if a.Temperature == nil {
				a.Temperature = &zero
			}
			if a.MaxTurns == 0 {
				a.MaxTurns = m.Defaults.Agent.MaxTurns
			}
			if a.MaxTurns == 0 {
				// Tool-less states make one model call per turn — 2 is
				// headroom; only a tool-use loop needs room to iterate.
				if len(a.Tools) > 0 {
					a.MaxTurns = DefaultMaxTurns
				} else {
					a.MaxTurns = DefaultMaxTurnsToolless
				}
			}
			if a.MaxOutputTokens == 0 {
				a.MaxOutputTokens = m.Defaults.Agent.MaxOutputTokens
			}
			if a.MaxOutputTokens == 0 {
				a.MaxOutputTokens = DefaultMaxOutputTokens
			}
			// maxInputTokens: nil cascades to the engine default; an author's
			// 0 means off. Never cascades onto implicit distill states — the
			// distiller is the one place the big payload is supposed to appear.
			if a.MaxInputTokens == nil && !s.IsDistill() {
				a.MaxInputTokens = m.Defaults.Agent.MaxInputTokens
			}
			if a.MaxInputTokens == nil && !s.IsDistill() {
				cap := DefaultMaxInputTokens
				a.MaxInputTokens = &cap
			}
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

		if f := s.ForEach; f != nil {
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

		// Retry: nil means unset -> machine defaults.retry -> engine default.
		// An explicit empty slice (retry: none) stays empty.
		if s.Retry == nil {
			if m.Defaults.Retry != nil {
				s.Retry = m.Defaults.Retry
			} else {
				s.Retry = DefaultRetryPolicies()
			}
		}
	}
}
