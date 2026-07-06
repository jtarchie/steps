package machine

import (
	"errors"
	"fmt"
	"math"
	"sort"
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

	validateNameCollisions(m, fail)
	validateWebhook(m, fail)
	validateModels(m, fail)

	for _, s := range m.States {
		validateState(m, s, cfg, fail)
	}

	validateReachability(m, fail)

	// distill sources must be run inputs or graph-predecessors; keys must not
	// collide with anything already in the flat scope (unless shadowing).
	for _, s := range m.States {
		validateDistill(m, s, fail)
	}

	// history.from / adopt targets must be graph-predecessors.
	for _, s := range m.States {
		if s.Agent == nil {
			continue
		}
		validateAdoptTarget(m, s, fail)
		validateHistoryFrom(m, s, fail)
	}

	return errors.Join(errs...)
}

// validateWebhook checks a trigger-only webhook: block. The map is a
// function of scope (payload + hook inputs) that returns run inputs; the
// dry-run (DryRun) proves it only reads declared inputs.
func validateWebhook(m *Machine, fail func(string, ...any)) {
	w := m.Webhook
	if w == nil {
		return
	}
	for _, r := range w.Path {
		ok := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			fail("webhook.path %q must be a URL-safe slug (lowercase letters, digits, - and _)", w.Path)
			break
		}
	}
	if w.Map.IsZero() {
		fail("webhook needs map — a function of ({body, headers, query, ...hook inputs}) returning run inputs")
		return
	}
	if !w.Map.IsFn() {
		fail("webhook.map must be a function of scope, not a literal value")
	}
	if w.MaxInFlight < 0 {
		fail("webhook.maxInFlight %d must be >= 0 (0 = default)", w.MaxInFlight)
	}
	if w.MaxQueued < 0 {
		fail("webhook.maxQueued %d must be >= 0 (0 = default)", w.MaxQueued)
	}
}

// validateModels checks each models: tier: a provider-namespaced (or "mock")
// ref, a known reasoning level, and a non-negative token cap. Per-state model
// refs are validated where they resolve (validateAgentModel); this covers tiers
// that no state happens to select.
func validateModels(m *Machine, fail func(string, ...any)) {
	for name, spec := range m.Models {
		if spec.Ref == "" {
			fail("models.%s: a tier needs a model", name)
		} else if !strings.Contains(spec.Ref, "/") && spec.Ref != "mock" {
			fail("models.%s: model %q must be provider-namespaced (e.g. anthropic/claude-haiku-4-5) or \"mock\"", name, spec.Ref)
		}
		switch spec.Reasoning {
		case "", "low", "medium", "high":
		default:
			fail("models.%s: unknown reasoning %q — valid: low, medium, high", name, spec.Reasoning)
		}
		if spec.MaxOutputTokens < 0 {
			fail("models.%s: maxOutputTokens %d must be >= 0", name, spec.MaxOutputTokens)
		}
	}
}

// validateNameCollisions checks that inputs and states — the flat scope's
// first-class names — never shadow engine keys or each other. User state
// names must also be destructurable identifiers, which keeps the lowered
// `name#key` namespace collision-free by construction (# cannot appear in
// an identifier).
func validateNameCollisions(m *Machine, fail func(string, ...any)) {
	for name := range m.Input {
		if contains(scopeReserved, name) {
			fail("input %q shadows a reserved scope key (%s)", name, strings.Join(scopeReserved, ", "))
		}
		if m.State(name) != nil {
			fail("input %q collides with a state of the same name", name)
		}
	}
	for _, s := range m.States {
		if contains(scopeReserved, s.Name) {
			fail("state %q shadows a reserved scope key (%s)", s.Name, strings.Join(scopeReserved, ", "))
		}
		if !s.IsDistill() && !s.Gate && !isIdentifier(s.Name) {
			fail("state %q: names must be valid identifiers (letters, digits, _)", s.Name)
		}
	}
}

// validateReachability checks that every non-terminal state is reachable
// from initial, and that a terminal state is reachable from every state
// that is itself reachable (implicit terminals and catch-only targets are
// excused when unreferenced).
func validateReachability(m *Machine, fail func(string, ...any)) {
	reach := reachableFrom(m, m.Initial)
	for _, s := range m.States {
		if s.Terminal {
			continue // done/failed may be reached from anywhere or not at all
		}
		if !reach[s.Name] {
			fail("state %q is unreachable from initial %q", s.Name, m.Initial)
		}
	}
	for _, s := range m.States {
		if s.Terminal || !reach[s.Name] {
			continue
		}
		if !reachesTerminal(m, s.Name) {
			fail("no terminal state is reachable from %q", s.Name)
		}
	}
}

func validateAdoptTarget(m *Machine, s *State, fail func(string, ...any)) {
	a := s.Agent.Adopt
	if a == "" || a == "self" {
		return
	}
	if m.State(a) == nil {
		fail("state %q adopts unknown state %q", s.Name, a)
	} else if !reachableFrom(m, a)[s.Name] {
		fail("state %q adopts %q, which is not a predecessor", s.Name, a)
	}
}

func validateHistoryFrom(m *Machine, s *State, fail func(string, ...any)) {
	h := s.Agent.History
	if h == nil {
		return
	}
	if m.State(h.From) == nil {
		fail("state %q history.from unknown state %q", s.Name, h.From)
	} else if !reachableFrom(m, h.From)[s.Name] {
		fail("state %q history.from %q, which is not a predecessor", s.Name, h.From)
	}
	if bad := scopeNameCollision(m, h.As); bad != "" {
		fail("state %q: history.as %q shadows %s in the scope", s.Name, h.As, bad)
	}
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
	if s.Terminal {
		validateTerminalState(s, fail)
		return
	}
	if !validateHandlerCount(s, fail) {
		return
	}

	validateStateModifiers(s, fail)
	if f := s.ForEach; f != nil {
		validateForEach(m, s, f, fail)
	}
	if p := s.Parallel; p != nil {
		validateParallel(m, s, p, fail)
	}
	if a := s.Agent; a != nil {
		validateAgent(m, s, a, cfg, fail)
	}
	if s.Action != nil {
		if cfg.toolChecker != nil && !cfg.toolChecker(s.Action.Name) {
			fail("state %q: action %q is not registered", s.Name, s.Action.Name)
		}
	}
	if h := s.Human; h != nil {
		validateHuman(m, s, h, fail)
	}
	validateInputShape(s, fail)

	if !validateTransitionsPresent(s, fail) {
		return
	}
	validateTransitions(m, s, fail)
	validateCatch(m, s, fail)
	validateRetryPolicies(s, fail)
}

// validateStateModifiers checks the shared modifiers whose handler is
// constrained: memo skips a replay (agent-only, actions have side effects) and
// verdict is an acceptance test over output (only handlers that produce one).
func validateStateModifiers(s *State, fail func(string, ...any)) {
	if s.Memo && s.Agent == nil {
		fail("state %q: memo is only supported on agent states — skipping an action would skip its side effects", s.Name)
	}
	if !s.Verdict.IsZero() && s.Agent == nil && s.Action == nil {
		fail("state %q: verdict is an acceptance test for a state that produces output — only agent or action states have one", s.Name)
	}
}

// validateInputShape checks that a non-function static input is an object —
// the only shape a state's input can merge into the run context.
func validateInputShape(s *State, fail func(string, ...any)) {
	if !s.Input.IsZero() && !s.Input.IsFn() {
		if _, ok := s.Input.Static.(map[string]any); !ok {
			fail("state %q: input must be an object or a function returning one, got %T", s.Name, s.Input.Static)
		}
	}
}

func validateTerminalState(s *State, fail func(string, ...any)) {
	if s.Agent != nil || s.Action != nil || s.Human != nil {
		fail("state %q: terminal states cannot have a handler", s.Name)
	}
	if s.Status != "" && s.Status != "failed" {
		fail("state %q: status must be \"failed\" or omitted", s.Name)
	}
}

// validateHandlerCount reports whether s has exactly one handler; false
// means the caller should stop validating this state further.
func validateHandlerCount(s *State, fail func(string, ...any)) bool {
	handlers := 0
	for _, h := range []bool{s.Agent != nil, s.Action != nil, s.Human != nil, s.Parallel != nil} {
		if h {
			handlers++
		}
	}
	if handlers != 1 {
		fail("state %q: needs exactly one handler (agent, action, human, or parallel), found %d", s.Name, handlers)
		return false
	}
	return true
}

func validateForEach(m *Machine, s *State, f *ForEachSpec, fail func(string, ...any)) {
	if s.Human != nil {
		fail("state %q: foreach cannot wrap a human gate", s.Name)
	}
	if f.Over.IsZero() {
		fail("state %q: foreach needs over (a function of scope returning a list)", s.Name)
	} else if !f.Over.IsFn() {
		fail("state %q: foreach.over must be a function of scope returning a list", s.Name)
	}
	if f.Concurrency < 1 {
		fail("state %q: foreach.concurrency must be >= 1", s.Name)
	}
	if f.OnItemFailure != "fail" && f.OnItemFailure != "skip" {
		fail("state %q: foreach.on_item_failure must be fail or skip, got %q", s.Name, f.OnItemFailure)
	}
	if bad := scopeNameCollision(m, f.As); bad != "" {
		fail("state %q: forEach.as %q shadows %s in the scope", s.Name, f.As, bad)
	}
	if len(s.Output.Events) > 0 {
		fail("state %q: foreach states cannot declare events — items produce no single event; route with guards over ctx.%s.items", s.Name, s.Name)
	}
	if s.Agent != nil && (s.Agent.Adopt != "" || s.Agent.History != nil) {
		fail("state %q: foreach states cannot use adopt/history — items are hermetic by design", s.Name)
	}
}

// validateParallel checks a fork state: at least two branches, sane knobs, no
// events, unique identifier labels, and — the v1 hermeticity ban — no human
// gate reachable in a branch and no adopt/history reaching across the fork
// boundary (a branch runs as a sub-run with the pre-fork scope snapshot, not
// prior conversations).
func validateParallel(m *Machine, s *State, p *ParallelSpec, fail func(string, ...any)) {
	if s.ForEach != nil {
		fail("state %q: a parallel state cannot also be forEach — parallel replaces the handler", s.Name)
	}
	if len(p.Branches) < 2 {
		fail("state %q: parallel needs at least two branches, found %d", s.Name, len(p.Branches))
	}
	if p.Concurrency < 1 {
		fail("state %q: parallel.concurrency must be >= 1", s.Name)
	}
	if p.OnBranchFailure != "fail" && p.OnBranchFailure != "collect" {
		fail("state %q: parallel.onBranchFailure must be fail or collect, got %q", s.Name, p.OnBranchFailure)
	}
	if len(s.Output.Events) > 0 {
		fail("state %q: parallel states cannot declare events — branches produce no single event; route with guards over ctx.%s", s.Name, s.Name)
	}
	seen := map[string]bool{}
	for _, b := range p.Branches {
		if !isIdentifier(b.Label) {
			fail("state %q: parallel branch label %q must be a valid identifier (the join reads ctx.%s.%s)", s.Name, b.Label, s.Name, b.Label)
		}
		if seen[b.Label] {
			fail("state %q: duplicate parallel branch label %q", s.Name, b.Label)
		}
		seen[b.Label] = true
		validateBranchHermetic(m, s, b, fail)
	}
}

// validateBranchHermetic enforces that a branch sub-flow is self-contained: no
// human gate (a branch cannot park in v1) and no adopt/history pointing outside
// the branch (the child run receives the flat scope snapshot, not the parent's
// conversations).
func validateBranchHermetic(m *Machine, fork *State, b Branch, fail func(string, ...any)) {
	within := reachableFrom(m, b.Entry)
	for name := range within {
		st := m.State(name)
		if st == nil || st.Terminal {
			continue
		}
		if st.Human != nil {
			fail("state %q: parallel branch %q reaches human gate %q — a branch cannot park in v1; gate before or after the fork", fork.Name, b.Label, name)
		}
		a := st.Agent
		if a == nil {
			continue
		}
		if a.Adopt != "" && a.Adopt != "self" && !within[a.Adopt] {
			fail("state %q: parallel branch %q state %q adopts %q across the fork boundary — a branch gets the pre-fork scope snapshot, not prior conversations", fork.Name, b.Label, name, a.Adopt)
		}
		if a.History != nil && a.History.From != "" && !within[a.History.From] {
			fail("state %q: parallel branch %q state %q reads history from %q across the fork boundary — not available to a hermetic branch", fork.Name, b.Label, name, a.History.From)
		}
	}
}

func validateAgent(m *Machine, s *State, a *AgentSpec, cfg validateConfig, fail func(string, ...any)) {
	validateAgentModel(m, s, a, fail)
	validateAgentPromptAndSystem(s, a, fail)
	validateAgentOptions(s, a, fail)
	validateAgentTools(a, cfg, s, fail)
}

func validateAgentPromptAndSystem(s *State, a *AgentSpec, fail func(string, ...any)) {
	if a.Prompt.IsZero() && s.Input.IsZero() {
		fail("state %q: agent needs a prompt or an input block", s.Name)
	}
	if !a.Prompt.IsZero() && !a.Prompt.IsFn() {
		if _, ok := a.Prompt.Static.(string); !ok {
			fail("state %q: prompt must be a string or a function, got %T", s.Name, a.Prompt.Static)
		}
	}
	if !a.System.IsZero() && !a.System.IsFn() {
		if _, ok := a.System.Static.(string); !ok {
			fail("state %q: system must be a string or a function, got %T", s.Name, a.System.Static)
		}
	}
}

// validateAgentOptions checks structuredOutput, reasoning, token budgets,
// and toolChoice — the agent's scalar knobs.
func validateAgentOptions(s *State, a *AgentSpec, fail func(string, ...any)) {
	if a.StructuredOutput != "prompt" && a.StructuredOutput != "native" {
		fail("state %q: structured_output must be prompt or native, got %q", s.Name, a.StructuredOutput)
	}
	switch a.Reasoning {
	case "", "low", "medium", "high":
	default:
		fail("state %q: reasoning must be low, medium, or high, got %q", s.Name, a.Reasoning)
	}
	if a.MaxOutputTokens < 0 {
		fail("state %q: max_output_tokens must be positive", s.Name)
	}
	if a.MaxOutputTokens > math.MaxInt32 {
		fail("state %q: max_output_tokens exceeds the model API's int32 limit", s.Name)
	}
	if a.MaxInputTokens != nil && *a.MaxInputTokens < 0 {
		fail("state %q: max_input_tokens must be positive (0 disables the cap)", s.Name)
	}
	switch a.ToolChoice {
	case "auto":
	case "required", "one_of":
		fail("state %q: tool_choice %q is not implemented in v1", s.Name, a.ToolChoice)
	default:
		fail("state %q: unknown tool_choice %q", s.Name, a.ToolChoice)
	}
}

func validateAgentModel(m *Machine, s *State, a *AgentSpec, fail func(string, ...any)) {
	switch {
	case a.Model.IsFn():
		// routed at runtime; the dry-run exercises it
	case a.Model.IsZero() && s.IsDistill():
		fail("state %q: no model for the distiller — set distill.%s.model, a models.%s alias, or a machine default",
			s.DistillOf, s.DistillKey, DistillerAlias)
	case a.Model.IsZero():
		fail("state %q: no model (set agent.model, defaults.agent.model, or an engine default)", s.Name)
	default:
		ref, ok := a.Model.Static.(string)
		if !ok {
			fail("state %q: model must be a string or a function, got %T", s.Name, a.Model.Static)
			return
		}
		if strings.Contains(ref, "/") || ref == "mock" {
			return
		}
		hint := "e.g. anthropic/claude-haiku-4-5"
		if len(m.Models) > 0 {
			aliases := make([]string, 0, len(m.Models))
			for k := range m.Models {
				aliases = append(aliases, k)
			}
			sort.Strings(aliases)
			hint = fmt.Sprintf("or one of the models: aliases (%s)", strings.Join(aliases, ", "))
		}
		fail("state %q: unknown model %q — must be provider-namespaced, %s", s.Name, ref, hint)
	}
}

func validateAgentTools(a *AgentSpec, cfg validateConfig, s *State, fail func(string, ...any)) {
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
		validateOneTool(cfg, tr, s, fail)
		validateToolRequire(a, tr, seen, s, fail)
	}
}

func validateOneTool(cfg validateConfig, tr ToolRef, s *State, fail func(string, ...any)) {
	if cfg.toolChecker != nil && !cfg.toolChecker(tr.Name) {
		fail("state %q: tool %q is not registered", s.Name, tr.Name)
	}
	if tr.OnReject != "feedback" && tr.OnReject != "fail" {
		fail("state %q: tool %q: onReject must be feedback or fail", s.Name, tr.Name)
	}
	if !tr.When.IsZero() && !tr.When.IsFn() {
		fail("state %q: tool %q: when must be a function of scope", s.Name, tr.Name)
	}
	if !tr.Args.IsZero() && !tr.Args.IsFn() {
		if _, ok := tr.Args.Static.(map[string]any); !ok {
			fail("state %q: tool %q: args must be an object or a function returning one", s.Name, tr.Name)
		}
	}
}

// validateToolRequire checks that a tool's require: names another tool
// actually attached to this state.
func validateToolRequire(a *AgentSpec, tr ToolRef, seen map[string]bool, s *State, fail func(string, ...any)) {
	if tr.Require == "" || seen[tr.Require] {
		return
	}
	for _, other := range a.Tools {
		if other.Name == tr.Require {
			return
		}
	}
	fail("state %q: tool %q requires %q, which is not attached", s.Name, tr.Name, tr.Require)
}

func validateHuman(m *Machine, s *State, h *HumanSpec, fail func(string, ...any)) {
	if h.Prompt.IsZero() {
		fail("state %q: human gate needs a prompt", s.Name)
	}
	if h.OnTimeout != "" && m.State(h.OnTimeout) == nil {
		fail("state %q: timeout route target %q does not exist", s.Name, h.OnTimeout)
	}
	if h.Timeout > 0 && h.OnTimeout == "" {
		fail("state %q: human timeout duration set but the gate's branch has no timeout: route", s.Name)
	}
	if h.OnTimeout != "" && h.Timeout == 0 {
		fail("state %q: gate has a timeout: route but no timeout duration on the state", s.Name)
	}
	if c := h.Choices; c != nil {
		validateHumanChoices(s, c, fail)
	}
}

func validateHumanChoices(s *State, c *ChoiceSpec, fail func(string, ...any)) {
	// The gate's resume-event alphabet is its transition on: values (flow
	// wiring has already populated Transitions).
	alphabet := []string{}
	for _, t := range s.Transitions {
		if t.On != "" {
			alphabet = append(alphabet, t.On)
		}
	}
	switch c.Kind {
	case "single":
		for _, opt := range c.Options {
			if !contains(alphabet, opt.Event) {
				fail("state %q: choice %q is not a resume event of this gate — branch keys: %v", s.Name, opt.Event, alphabet)
			}
		}
	case "multi":
		if c.Event == "" {
			switch len(alphabet) {
			case 1:
				c.Event = alphabet[0]
			default:
				fail("state %q: multi choices need event: — the gate's branch has %d event edges %v", s.Name, len(alphabet), alphabet)
			}
		} else if !contains(alphabet, c.Event) {
			fail("state %q: multi choices event %q is not a resume event of this gate — branch keys: %v", s.Name, c.Event, alphabet)
		}
		if c.Min < 0 {
			fail("state %q: choices min must be >= 0", s.Name)
		}
		if c.Max != 0 && c.Max < c.Min {
			fail("state %q: choices max (%d) must be >= min (%d)", s.Name, c.Max, c.Min)
		}
	}
}

// validateTransitionsPresent reports whether s has any transitions; false
// means the caller should stop validating this state further.
func validateTransitionsPresent(s *State, fail func(string, ...any)) bool {
	if len(s.Transitions) == 0 {
		fail("state %q: non-terminal state has no transitions (defaults should have filled this)", s.Name)
		return false
	}
	return true
}

// validateTransitions checks targets exist, events are declared, and the
// fallback edge (if any) is last.
func validateTransitions(m *Machine, s *State, fail func(string, ...any)) {
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
}

func validateCatch(m *Machine, s *State, fail func(string, ...any)) {
	for i, c := range s.Catch {
		if m.State(c.To) == nil {
			fail("state %q: catch %d targets unknown state %q", s.Name, i, c.To)
		}
		if len(c.Match) == 0 {
			fail("state %q: catch %d has no match classes (use [\"*\"])", s.Name, i)
		}
	}
}

func validateRetryPolicies(s *State, fail func(string, ...any)) {
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

// validateDistill checks a state's distill entries: for: present, budgets
// sane, keys collision-free, and every source a run input or a
// graph-predecessor (mirroring history.from). Runs after lowering, so
// reachability includes the implicit chains.
func validateDistill(m *Machine, s *State, fail func(string, ...any)) {
	if len(s.Distill) == 0 {
		return
	}
	if s.Terminal {
		fail("state %q: terminal states cannot distill", s.Name)
		return
	}
	for i := range s.Distill {
		d := &s.Distill[i]
		where := fmt.Sprintf("state %q distill.%s", s.Name, d.Key)
		validateDistillEntryShape(s, d, where, fail)
		validateDistillKeyCollisions(m, s, d, where, fail)
		validateDistillSource(m, s, d, where, fail)
	}
}

// validateDistillEntryShape checks for:, maxTokens, and that the slice fits
// under the consumer's input budget (a slice that does not fit only blows
// the cap it was meant to protect).
func validateDistillEntryShape(s *State, d *DistillEntry, where string, fail func(string, ...any)) {
	if d.For.IsZero() {
		fail("%s: needs for: (what this state needs from the source)", where)
	} else if !d.For.IsFn() {
		if _, ok := d.For.Static.(string); !ok {
			fail("%s: for must be a string or a function of scope, got %T", where, d.For.Static)
		}
	}
	if d.MaxTokens < 0 {
		fail("%s: maxTokens must be positive", where)
	}
	if a := s.Agent; a != nil && a.MaxInputTokens != nil && *a.MaxInputTokens > 0 && d.MaxTokens >= *a.MaxInputTokens {
		fail("%s: maxTokens %d does not fit under the consumer's maxInputTokens %d — a slice must be smaller than its consumer's input budget",
			where, d.MaxTokens, *a.MaxInputTokens)
	}
}

// validateDistillKeyCollisions checks that a distill entry's key does not
// shadow anything else already at the flat scope root.
func validateDistillKeyCollisions(m *Machine, s *State, d *DistillEntry, where string, fail func(string, ...any)) {
	if contains(scopeReserved, d.Key) {
		fail("%s: key shadows a reserved scope key (%s)", where, strings.Join(scopeReserved, ", "))
	}
	if f := s.ForEach; f != nil && f.As == d.Key {
		fail("%s: key collides with forEach.as %q", where, f.As)
	}
	if a := s.Agent; a != nil && a.History != nil && a.History.As == d.Key {
		fail("%s: key collides with history.as %q", where, a.History.As)
	}
	if d.From != d.Key { // derived name: must not shadow anything real
		if bad := scopeNameCollision(m, d.Key); bad != "" {
			fail("%s: introduces %q, which shadows %s", where, d.Key, bad)
		}
	}
}

// validateDistillSource checks that the entry's source is a run input or a
// graph-predecessor (mirroring history.from).
func validateDistillSource(m *Machine, s *State, d *DistillEntry, where string, fail func(string, ...any)) {
	if contains(scopeReserved, d.From) {
		fail("%s: cannot distill engine key %q", where, d.From)
		return
	}
	if _, isInput := m.Input[d.From]; isInput {
		return
	}
	if src := m.State(d.From); src != nil {
		if !reachableFrom(m, d.From)[s.Name] {
			fail("%s: source state %q is not a predecessor", where, d.From)
		}
	} else if len(m.Input) > 0 {
		// Without an input: block, run input keys are unknowable — declaring
		// input: buys strict source checking too.
		fail("%s: source %q is not a run input or a predecessor state", where, d.From)
	}
}

// isIdentifier reports whether a name is a valid JS identifier — the flat
// scope requires destructurable names, and the `#` in lowered distill names
// stays collision-free only because user names can never contain it.
func isIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '$':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// scopeNameCollision reports what a proposed scope name (forEach.as,
// history.as) would shadow — engine keys, run inputs, or state names all
// live at the flat scope root.
func scopeNameCollision(m *Machine, name string) string {
	if contains(scopeReserved, name) {
		return "a reserved engine key"
	}
	if _, ok := m.Input[name]; ok {
		return "a run input"
	}
	if m.State(name) != nil {
		return "a state's output"
	}
	return ""
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
	// A fork reaches its branch entries — the reachability seed that makes
	// branch closures reachable from initial (and each branch state's pre-fork
	// predecessors visible to adopt/history/distill and the dry-run scope).
	if s.Parallel != nil {
		for _, b := range s.Parallel.Branches {
			out = append(out, b.Entry)
		}
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
