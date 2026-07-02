package machine

import (
	"errors"
	"fmt"
	"strings"
)

// ToolChecker lets the caller verify tool/action names against a registry.
// Nil skips the check (e.g. library users who register tools after load).
type ToolChecker func(name string) bool

// Validate enforces the load-time guarantees from DESIGN.md. It must run
// after ApplyDefaults and Compile, so the machine is fully explicit.
func Validate(m *Machine, opts ...ValidateOption) error {
	cfg := validateConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if m.Name == "" {
		fail("machine has no name")
	}
	if len(m.States) == 0 {
		fail("machine has no states")
	}
	if m.State(m.Initial) == nil {
		fail("initial state %q does not exist", m.Initial)
	}

	for _, s := range m.States {
		validateState(m, s, cfg, fail)
	}

	// Reachability: every state reachable from initial (implicit terminals
	// and catch-only targets excused when unreferenced is fine — they must
	// still be *declared-reachable* if user-declared and non-terminal).
	reach := reachableFrom(m, m.Initial)
	for _, s := range m.States {
		if s.Terminal {
			continue // done/failed may be reached from anywhere or not at all
		}
		if !reach[s.Name] {
			fail("state %q is unreachable from initial %q", s.Name, m.Initial)
		}
	}

	// Termination: a terminal state must be reachable from every state.
	for _, s := range m.States {
		if s.Terminal || !reach[s.Name] {
			continue
		}
		if !reachesTerminal(m, s.Name) {
			fail("no terminal state is reachable from %q", s.Name)
		}
	}

	// history.from / adopt targets must be graph-predecessors.
	for _, s := range m.States {
		if s.Agent == nil {
			continue
		}
		if a := s.Agent.Adopt; a != "" && a != "self" {
			if m.State(a) == nil {
				fail("state %q adopts unknown state %q", s.Name, a)
			} else if !reachableFrom(m, a)[s.Name] {
				fail("state %q adopts %q, which is not a predecessor", s.Name, a)
			}
		}
		if h := s.Agent.History; h != nil {
			if m.State(h.From) == nil {
				fail("state %q history.from unknown state %q", s.Name, h.From)
			} else if !reachableFrom(m, h.From)[s.Name] {
				fail("state %q history.from %q, which is not a predecessor", s.Name, h.From)
			}
		}
	}

	return errors.Join(errs...)
}

type validateConfig struct {
	toolChecker ToolChecker
}

// ValidateOption configures Validate.
type ValidateOption func(*validateConfig)

// WithToolChecker verifies action/tool names against a registry.
func WithToolChecker(tc ToolChecker) ValidateOption {
	return func(c *validateConfig) { c.toolChecker = tc }
}

func validateState(m *Machine, s *State, cfg validateConfig, fail func(string, ...any)) {
	// Exactly one handler, or terminal.
	handlers := 0
	for _, h := range []bool{s.Agent != nil, s.Action != nil, s.Human != nil} {
		if h {
			handlers++
		}
	}
	if s.Terminal {
		if handlers != 0 {
			fail("state %q: terminal states cannot have a handler", s.Name)
		}
		if s.Status != "" && s.Status != "failed" {
			fail("state %q: status must be \"failed\" or omitted", s.Name)
		}
		return
	}
	if handlers != 1 {
		fail("state %q: needs exactly one handler (agent, action, or human), found %d", s.Name, handlers)
		return
	}

	if a := s.Agent; a != nil {
		if a.Model == "" {
			fail("state %q: no model (set agent.model, defaults.agent.model, or an engine default)", s.Name)
		} else if !strings.Contains(a.Model, "/") && a.Model != "mock" {
			fail("state %q: model %q must be provider-namespaced, e.g. anthropic/claude-haiku-4-5", s.Name, a.Model)
		}
		if a.Prompt == "" && len(s.Input) == 0 {
			fail("state %q: agent needs a prompt or an input block", s.Name)
		}
		if a.Prompt != "" {
			if _, err := ParseTemplate(s.Name+".prompt", a.Prompt); err != nil {
				fail("state %q: %v", s.Name, err)
			}
		}
		if a.System != "" {
			if _, err := ParseTemplate(s.Name+".system", a.System); err != nil {
				fail("state %q: %v", s.Name, err)
			}
		}
		switch a.ToolChoice {
		case "auto":
		case "required", "one_of":
			fail("state %q: tool_choice %q is not implemented in v1", s.Name, a.ToolChoice)
		default:
			fail("state %q: unknown tool_choice %q", s.Name, a.ToolChoice)
		}
		seen := map[string]bool{}
		for _, tr := range a.Tools {
			if tr.Name == "" {
				fail("state %q: tool with no name", s.Name)
				continue
			}
			if seen[tr.Name] {
				fail("state %q: tool %q attached twice", s.Name, tr.Name)
			}
			seen[tr.Name] = true
			if cfg.toolChecker != nil && !cfg.toolChecker(tr.Name) {
				fail("state %q: tool %q is not registered", s.Name, tr.Name)
			}
			if tr.OnReject != "feedback" && tr.OnReject != "fail" {
				fail("state %q: tool %q: on_reject must be feedback or fail", s.Name, tr.Name)
			}
			if tr.Require != "" && !seen[tr.Require] {
				// require must reference another tool on this state
				found := false
				for _, other := range a.Tools {
					if other.Name == tr.Require {
						found = true
						break
					}
				}
				if !found {
					fail("state %q: tool %q requires %q, which is not attached", s.Name, tr.Name, tr.Require)
				}
			}
		}
	}

	if s.Action != nil {
		if cfg.toolChecker != nil && !cfg.toolChecker(s.Action.Name) {
			fail("state %q: action %q is not registered", s.Name, s.Action.Name)
		}
	}

	if h := s.Human; h != nil {
		if h.Prompt == "" {
			fail("state %q: human gate needs a prompt", s.Name)
		} else if _, err := ParseTemplate(s.Name+".human", h.Prompt); err != nil {
			fail("state %q: %v", s.Name, err)
		}
		if h.OnTimeout != "" && m.State(h.OnTimeout) == nil {
			fail("state %q: on_timeout target %q does not exist", s.Name, h.OnTimeout)
		}
		if h.Timeout > 0 && h.OnTimeout == "" {
			fail("state %q: human timeout set but no on_timeout target", s.Name)
		}
	}

	for k, v := range s.Input {
		if _, err := ParseTemplate(s.Name+".input."+k, v); err != nil {
			fail("state %q: input %q: %v", s.Name, k, err)
		}
	}

	// Transitions: targets exist; events declared; fallback last and present.
	if len(s.Transitions) == 0 {
		fail("state %q: non-terminal state has no transitions (defaults should have filled this)", s.Name)
		return
	}
	events := map[string]bool{}
	for _, e := range s.Output.Events {
		events[e] = true
	}
	// Human gates route on resume events that are not output events.
	humanGate := s.Human != nil
	for i, t := range s.Transitions {
		if m.State(t.To) == nil {
			fail("state %q: transition %d targets unknown state %q", s.Name, i, t.To)
		}
		if t.On != "" && !humanGate && !events[t.On] {
			fail("state %q: transition on %q, but %q is not in output.events %v", s.Name, t.On, t.On, s.Output.Events)
		}
		if t.Fallback() && i != len(s.Transitions)-1 {
			fail("state %q: transition %d is an unconditional fallback; transitions after it are unreachable", s.Name, i)
		}
	}
	if !s.Transitions[len(s.Transitions)-1].Fallback() && !humanGate {
		fail("state %q: last transition must be an unconditional fallback (no on/when)", s.Name)
	}

	for i, c := range s.Catch {
		if m.State(c.To) == nil {
			fail("state %q: catch %d targets unknown state %q", s.Name, i, c.To)
		}
		if len(c.Match) == 0 {
			fail("state %q: catch %d has no match classes (use [\"*\"])", s.Name, i)
		}
	}

	for _, rp := range s.Retry {
		if rp.MaxAttempts < 1 {
			fail("state %q: retry policy max_attempts must be >= 1", s.Name)
		}
		if rp.Backoff.Factor != 0 && rp.Backoff.Factor < 1 {
			fail("state %q: retry backoff factor must be >= 1", s.Name)
		}
		if len(rp.Match) == 0 {
			fail("state %q: retry policy has no match classes", s.Name)
		}
	}
}

// edges returns every state directly reachable from s: transitions, catch
// targets, and human timeout routes.
func edges(s *State) []string {
	var out []string
	for _, t := range s.Transitions {
		out = append(out, t.To)
	}
	for _, c := range s.Catch {
		out = append(out, c.To)
	}
	if s.Human != nil && s.Human.OnTimeout != "" {
		out = append(out, s.Human.OnTimeout)
	}
	return out
}

func reachableFrom(m *Machine, start string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		name := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		s := m.State(name)
		if s == nil {
			continue
		}
		stack = append(stack, edges(s)...)
	}
	return seen
}

func reachesTerminal(m *Machine, from string) bool {
	for name := range reachableFrom(m, from) {
		if s := m.State(name); s != nil && s.Terminal {
			return true
		}
	}
	return false
}
