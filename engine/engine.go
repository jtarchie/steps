// Package engine executes machines: the run loop, retry/catch policies,
// transition selection, budgets, and durability via the journal. Handlers
// (agent/action/human) plug into the loop; the agent handler drives ADK.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	mrand "math/rand"
	"slices"
	"sync"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
	"github.com/jtarchie/steps/toolreg"
)

// Engine runs machines durably against a Store.
type Engine struct {
	Store     journal.Store
	Providers *provider.Registry
	Tools     *toolreg.Registry
	Listener  Listener

	// Mock, when set, replaces every model with scripted responses.
	Mock provider.Script

	mocks map[string]*provider.Mock // per-run scripted queues

	llmMu sync.Mutex
	llms  map[string]adkmodel.LLM // resolved clients, keyed by model ref
}

// resolveLLM caches provider clients per model ref: states re-run and revisit
// constantly, and rebuilding HTTP clients per execution is pure waste.
func (e *Engine) resolveLLM(ref string) (adkmodel.LLM, error) {
	e.llmMu.Lock()
	defer e.llmMu.Unlock()
	if llm, ok := e.llms[ref]; ok {
		return llm, nil
	}
	llm, err := e.Providers.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if e.llms == nil {
		e.llms = map[string]adkmodel.LLM{}
	}
	e.llms[ref] = llm
	return llm, nil
}

// New builds an engine with sane defaults.
func New(store journal.Store, providers *provider.Registry, tools *toolreg.Registry, l Listener) *Engine {
	if l == nil {
		l = NopListener{}
	}
	return &Engine{Store: store, Providers: providers, Tools: tools, Listener: l}
}

// Result is the outcome of driving a run as far as it can go.
type Result struct {
	RunID    string
	Status   string // done | failed | parked
	Terminal string // terminal state name when finished
	State    *journal.RunState
}

// HandlerResult is what a state's handler produced.
type HandlerResult struct {
	Output   map[string]any
	Event    string
	Usage    journal.Usage
	Messages []journal.Message
	Attempts int  // handler attempts consumed (1 = first try)
	Memo     bool // output replayed from the memo cache — zero tokens spent
	// Passthrough: a distill source already fit its slice budget and crossed
	// verbatim — zero tokens, no model call.
	Passthrough bool
	Park        *parkRequest // human gates request a park instead of producing output
}

type parkRequest struct {
	Prompt    string
	Timeout   time.Duration
	OnTimeout string
	Choices   *journal.ParkChoices
}

type pendingResume struct {
	state string
	event string
	data  map[string]any
}

// Start validates input, creates the run, and drives it until it finishes or
// parks.
func (e *Engine) Start(ctx context.Context, m *machine.Machine, input map[string]any) (*Result, error) {
	for name, spec := range m.Input {
		if _, ok := input[name]; !ok && spec.Required {
			return nil, fmt.Errorf("missing required input %q", name)
		}
	}

	runID := newRunID()
	run := &journal.Run{
		ID:      runID,
		Machine: m.Name,
		Hash:    m.Hash,
		Source:  m.Source,
		Assets:  m.Assets,
		Status:  journal.StatusRunning,
	}
	if err := e.Store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	inputAny := make(map[string]any, len(input))
	for k, v := range input {
		inputAny[k] = v
	}
	if err := e.append(ctx, runID, journal.RunStarted, map[string]any{
		"machine_hash": m.Hash,
		"machine":      m.Name,
		"input":        inputAny,
		"initial":      m.Initial,
	}); err != nil {
		return nil, err
	}
	e.Listener.RunStarted(runID, m.Name, inputAny)

	rs := &journal.RunState{
		Ctx:     map[string]any{},
		Visits:  map[string]int{},
		Convos:  map[string][]journal.Message{},
		Started: time.Now(),
		Current: m.Initial,
	}
	for k, v := range inputAny {
		rs.Ctx[k] = v
	}
	return e.loop(ctx, m, runID, rs, false, nil)
}

// Resume continues a parked or crashed run. For parked human gates, event
// (and optional data) is the gate's answer; expired gates route to
// on_timeout regardless of the event.
func (e *Engine) Resume(ctx context.Context, m *machine.Machine, runID, event string, data map[string]any) (*Result, error) {
	events, err := e.Store.Events(ctx, runID)
	if err != nil {
		return nil, err
	}
	rs := journal.Fold(events)
	if rs.Finished {
		return nil, fmt.Errorf("run %s already finished (%s)", runID, rs.Status)
	}
	// Recompute wall-clock baseline: journal times survive the crash.
	if rs.Started.IsZero() {
		rs.Started = time.Now()
	}

	var pending *pendingResume
	inFlight := rs.InFlight

	if p := rs.Parked; p != nil {
		if p.Expired(time.Now()) {
			// Stale gate: route to on_timeout, ignoring the provided event.
			e.Listener.Warn("human gate expired; routing to on_timeout", "state", p.State, "to", p.OnTimeout)
			if err := e.append(ctx, runID, journal.RunResumed, map[string]any{"event": "timeout"}); err != nil {
				return nil, err
			}
			if _, err := e.fireTransition(ctx, m, runID, rs, p.State, p.OnTimeout, "timeout", ""); err != nil {
				return nil, err
			}
			return e.loop(ctx, m, runID, rs, false, nil)
		}
		if event == "" {
			return nil, fmt.Errorf("run %s is parked at human gate %q — resume with an event", runID, p.State)
		}
		// A typo'd --event (or a forged web POST) should fail here, not as a
		// mid-run "no transition matched": the gate's alphabet is closed.
		if st := m.State(p.State); st != nil {
			known, fallback := []string{}, false
			for _, t := range st.Transitions {
				if t.On != "" {
					known = append(known, t.On)
				}
				if t.Fallback() {
					fallback = true
				}
			}
			if !fallback && !slices.Contains(known, event) {
				return nil, fmt.Errorf("gate %q has no route for event %q — expected one of %v", p.State, event, known)
			}
		}
		if err := e.append(ctx, runID, journal.RunResumed, map[string]any{"event": event, "data": data}); err != nil {
			return nil, err
		}
		e.Listener.RunResumed(runID, event)
		pending = &pendingResume{state: p.State, event: event, data: data}
		inFlight = false
	}

	if err := e.Store.UpdateRun(ctx, runID, journal.StatusRunning, rs.Current); err != nil {
		return nil, err
	}
	return e.loop(ctx, m, runID, rs, inFlight, pending)
}

// loop drives the machine until a terminal state, a park, or an error.
// inFlight means the current state's state_entered is already journaled
// (crash-resume mid-state); pending carries a human gate's answer.
func (e *Engine) loop(ctx context.Context, m *machine.Machine, runID string, rs *journal.RunState, inFlight bool, pending *pendingResume) (*Result, error) {
	current := rs.Current
	if current == "" {
		current = m.Initial
	}

	// visits.<state> must be a number for EVERY state — a guard like
	// `visits.draft < 3` on a never-entered state reads undefined in JS,
	// and `undefined < 3` is false: a silent misroute.
	for _, s := range m.States {
		if _, ok := rs.Visits[s.Name]; !ok {
			rs.Visits[s.Name] = 0
		}
	}

	for {
		st := m.State(current)
		if st == nil {
			return nil, fmt.Errorf("run %s: unknown state %q", runID, current)
		}

		if st.Terminal {
			status := journal.StatusDone
			if st.Status == "failed" {
				status = journal.StatusFailed
			}
			if err := e.append(ctx, runID, journal.RunFinished, map[string]any{
				"terminal_state": st.Name,
				"status":         status,
			}); err != nil {
				return nil, err
			}
			if err := e.Store.UpdateRun(ctx, runID, status, st.Name); err != nil {
				return nil, err
			}
			e.Listener.RunFinished(runID, status, st.Name, rs.Transitions, rs.Usage)
			return &Result{RunID: runID, Status: status, Terminal: st.Name, State: rs}, nil
		}

		// Run-level wall clock.
		if time.Since(rs.Started) > m.Limits.Timeout {
			next, err := e.routeFailure(ctx, m, runID, rs, st, machine.ClassRunTimeout, fmt.Errorf("run exceeded %s", m.Limits.Timeout))
			if err != nil {
				return nil, err
			}
			current = next
			continue
		}

		enteredAt := time.Now()
		var res *HandlerResult
		var runErr error

		if pending != nil && pending.state == current {
			res = &HandlerResult{Output: pending.data, Event: pending.event}
			if res.Output == nil {
				res.Output = map[string]any{}
			}
			pending = nil
		} else {
			if !inFlight {
				rs.Visits[current]++
				if err := e.append(ctx, runID, journal.StateEntered, map[string]any{
					"state": current,
					"visit": rs.Visits[current],
				}); err != nil {
					return nil, err
				}
			}
			inFlight = false
			model := ""
			if st.Agent != nil {
				model = st.Agent.Model.Display()
			}
			e.Listener.StateEntered(current, st.HandlerKind(), rs.Visits[current], model)
			if err := e.Store.UpdateRun(ctx, runID, journal.StatusRunning, current); err != nil {
				return nil, err
			}

			res, runErr = e.runHandler(ctx, m, st, runID, rs)
			if runErr != nil {
				next, err := e.routeFailure(ctx, m, runID, rs, st, provider.Classify(runErr), runErr)
				if err != nil {
					return nil, err
				}
				current = next
				continue
			}
		}

		if res.Park != nil {
			parked := map[string]any{
				"state":      current,
				"reason":     "human_gate",
				"prompt":     res.Park.Prompt,
				"timeout":    res.Park.Timeout,
				"on_timeout": res.Park.OnTimeout,
			}
			if res.Park.Choices != nil {
				parked["choices"] = res.Park.Choices
			}
			if err := e.append(ctx, runID, journal.RunParked, parked); err != nil {
				return nil, err
			}
			if err := e.Store.UpdateRun(ctx, runID, journal.StatusParked, current); err != nil {
				return nil, err
			}
			// Mirror the journal into the in-memory state so same-process
			// callers (interactive gates, tests) see the park without re-folding.
			rs.Parked = &journal.ParkInfo{
				State:     current,
				Reason:    "human_gate",
				Prompt:    res.Park.Prompt,
				At:        time.Now(),
				Timeout:   res.Park.Timeout,
				OnTimeout: res.Park.OnTimeout,
				Choices:   res.Park.Choices,
			}
			e.Listener.RunParked(runID, current, res.Park.Prompt, res.Park.Timeout, res.Park.Choices)
			return &Result{RunID: runID, Status: journal.StatusParked, State: rs}, nil
		}

		// Merge the state's conclusion into ctx and journal it.
		rs.Ctx[current] = res.Output
		rs.Usage.Add(res.Usage)
		if len(res.Messages) > 0 {
			rs.Convos[current] = res.Messages
		}
		finished := map[string]any{
			"state":    current,
			"output":   res.Output,
			"event":    res.Event,
			"usage":    res.Usage,
			"messages": res.Messages,
		}
		if res.Memo {
			finished["memo"] = true
		}
		if res.Passthrough {
			finished["passthrough"] = true
		}
		if err := e.append(ctx, runID, journal.HandlerFinished, finished); err != nil {
			return nil, err
		}
		e.Listener.HandlerFinished(current, res.Output, res.Event, res.Usage)

		// Token/cost budgets.
		if class, exceeded := e.checkBudgets(m, rs); exceeded {
			next, err := e.routeFailure(ctx, m, runID, rs, st, class, fmt.Errorf("budget exceeded: %s", class))
			if err != nil {
				return nil, err
			}
			current = next
			continue
		}

		// Transitions: event match AND guard, in order, first match wins.
		tr, err := e.pickTransition(st, res, rs, enteredAt)
		if err != nil {
			next, rerr := e.routeFailure(ctx, m, runID, rs, st, machine.ClassGuardRejected, err)
			if rerr != nil {
				return nil, rerr
			}
			current = next
			continue
		}
		next, err := e.fireTransition(ctx, m, runID, rs, current, tr.To, tr.On, tr.When.Display())
		if err != nil {
			return nil, err
		}
		current = next
	}
}

// fireTransition journals a transition and enforces the cycle guard. Hops
// out of implicit distill states don't count toward maxTransitions — loop
// bounds are properties of the authored topology, and each distill chain is
// authored as part of its consumer.
func (e *Engine) fireTransition(ctx context.Context, m *machine.Machine, runID string, rs *journal.RunState, from, to, on, when string) (string, error) {
	implicit := false
	if fs := m.State(from); fs != nil && fs.IsDistill() {
		implicit = true
	}
	if !implicit && rs.Transitions >= m.Limits.MaxTransitions {
		// The cycle guard itself cannot be caught into a loop: go straight
		// to the failed terminal.
		e.Listener.Warn("max_transitions reached; failing run", "limit", m.Limits.MaxTransitions)
		to, on, when = "failed", "max_transitions", ""
	}
	data := map[string]any{"from": from, "to": to, "on": on, "guard": when}
	if implicit {
		data["implicit"] = true
	}
	if err := e.append(ctx, runID, journal.TransitionFired, data); err != nil {
		return "", err
	}
	if !implicit {
		rs.Transitions++
	}
	rs.Current = to
	e.Listener.TransitionFired(from, to, on, when)
	return to, nil
}

// routeFailure sends an unrecoverable handler failure through catch, or to
// the failed terminal.
func (e *Engine) routeFailure(ctx context.Context, m *machine.Machine, runID string, rs *journal.RunState, st *machine.State, class string, cause error) (string, error) {
	e.Listener.HandlerFailed(st.Name, class, cause, 0)
	to := "failed"
	for _, c := range st.Catch {
		if c.Matches(class) {
			to = c.To
			break
		}
	}
	return e.fireTransition(ctx, m, runID, rs, st.Name, to, "catch:"+class, "")
}

func (e *Engine) checkBudgets(m *machine.Machine, rs *journal.RunState) (string, bool) {
	if m.Limits.MaxTokens > 0 && rs.Usage.Total() > m.Limits.MaxTokens {
		return machine.ClassBudgetExceeded, true
	}
	if m.Limits.MaxCost > 0 && rs.Usage.Cost > m.Limits.MaxCost {
		return machine.ClassBudgetExceeded, true
	}
	return "", false
}

// baseScope builds the FLAT scope every machine function receives: run
// inputs and state outputs at the root (destructure what you need), plus
// the engine keys. Collisions with reserved names are rejected at load.
func baseScope(rs *journal.RunState) map[string]any {
	scope := make(map[string]any, len(rs.Ctx)+3)
	for k, v := range rs.Ctx {
		scope[k] = v
	}
	scope["visits"] = rs.Visits
	scope["run"] = map[string]any{
		"transitions": rs.Transitions,
		"tokens":      rs.Usage.Total(),
		"cost":        rs.Usage.Cost,
	}
	scope["attempt"] = 1
	return scope
}

// pickTransition evaluates the state's transitions in order.
func (e *Engine) pickTransition(st *machine.State, res *HandlerResult, rs *journal.RunState, enteredAt time.Time) (machine.Transition, error) {
	scope := baseScope(rs)
	scope["output"] = res.Output
	scope["event"] = res.Event
	if res.Attempts > 0 {
		scope["attempt"] = res.Attempts
	}

	for _, t := range st.Transitions {
		if t.On != "" && t.On != res.Event {
			continue
		}
		if !t.When.IsZero() {
			ok, err := t.When.Bool(scope)
			if err != nil {
				e.Listener.Warn("guard evaluation failed; treating as false",
					"state", st.Name, "guard", t.When.Display(), "error", err.Error())
				continue
			}
			if !ok {
				continue
			}
		}
		return t, nil
	}
	return machine.Transition{}, fmt.Errorf("state %q: no transition matched event %q", st.Name, res.Event)
}

// runHandler dispatches to the state's handler with the transient retry
// policy wrapped around it. Semantic (schema) retries happen inside the agent
// handler, where the conversation lives.
func (e *Engine) runHandler(ctx context.Context, m *machine.Machine, st *machine.State, runID string, rs *journal.RunState) (*HandlerResult, error) {
	if st.Human != nil {
		return e.runHuman(st, rs)
	}
	if st.ForEach != nil {
		return e.runForEach(ctx, m, st, runID, rs)
	}
	return e.withRetries(ctx, st, runID, func(attempt int) (*HandlerResult, error) {
		return e.runOnce(ctx, m, st, runID, rs, nil, attempt)
	})
}

// runOnce executes the state's body a single time with optional extra
// template data (foreach items).
func (e *Engine) runOnce(ctx context.Context, m *machine.Machine, st *machine.State, runID string, rs *journal.RunState, extra map[string]any, attempt int) (*HandlerResult, error) {
	switch {
	case st.Action != nil:
		return e.runAction(ctx, st, rs, extra, attempt)
	case st.Agent != nil:
		return e.runAgent(ctx, m, st, runID, rs, extra, attempt)
	}
	return nil, fmt.Errorf("state %q has no handler", st.Name)
}

// maxForEachItems is a runaway backstop, far above any sane fan-out.
const maxForEachItems = 1000

// runForEach fans the state's handler out over a list evaluated from ctx.
// Each item runs hermetically (agents: a fresh conversation per item — N
// small context windows instead of one big one) under the state's retry
// policy. Output: {items: [per-item outputs], count}. Sequential in v1.
func (e *Engine) runForEach(ctx context.Context, m *machine.Machine, st *machine.State, runID string, rs *journal.RunState) (*HandlerResult, error) {
	list, err := st.ForEach.Over.List(baseScope(rs))
	if err != nil {
		return nil, &provider.ClassifiedError{Class: machine.ClassActionError,
			Msg: fmt.Sprintf("foreach.over: %v", err)}
	}
	if len(list) > maxForEachItems {
		return nil, &provider.ClassifiedError{Class: machine.ClassBudgetExceeded,
			Msg: fmt.Sprintf("foreach over %d items exceeds the %d backstop", len(list), maxForEachItems)}
	}

	runItem := func(i int, item any) (*HandlerResult, error) {
		e.Listener.ForEachItem(st.Name, i, len(list), item)
		extra := map[string]any{
			st.ForEach.As: item,
			"index":       i,
			"total":       len(list),
		}
		return e.withRetries(ctx, st, runID, func(attempt int) (*HandlerResult, error) {
			return e.runOnce(ctx, m, st, runID, rs, extra, attempt)
		})
	}

	// Bounded concurrency; mock runs stay sequential so scripted queues
	// remain deterministic.
	concurrency := st.ForEach.Concurrency
	if concurrency < 1 || e.Mock != nil {
		concurrency = 1
	}
	results := make([]*HandlerResult, len(list))
	errs := make([]error, len(list))
	if concurrency == 1 {
		for i, item := range list {
			results[i], errs[i] = runItem(i, item)
		}
	} else {
		var wg sync.WaitGroup
		sem := make(chan struct{}, concurrency)
		for i, item := range list {
			wg.Add(1)
			go func(i int, item any) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				results[i], errs[i] = runItem(i, item)
			}(i, item)
		}
		wg.Wait()
	}

	items := make([]any, 0, len(list))
	var failures []any
	var usage journal.Usage
	memoHits := 0
	passthroughHits := 0
	for i := range list {
		if errs[i] != nil {
			if st.ForEach.OnItemFailure == "skip" {
				e.Listener.Warn("foreach item skipped", "state", st.Name,
					"item", fmt.Sprintf("%d/%d", i+1, len(list)), "error", errs[i].Error())
				failures = append(failures, map[string]any{
					"index": i,
					"class": provider.Classify(errs[i]),
					"error": errs[i].Error(),
				})
				continue
			}
			return nil, fmt.Errorf("item %d/%d: %w", i+1, len(list), errs[i])
		}
		items = append(items, results[i].Output)
		usage.Add(results[i].Usage)
		if results[i].Memo {
			memoHits++
		}
		if results[i].Passthrough {
			passthroughHits++
		}
	}
	output := map[string]any{"items": items, "count": len(items)}
	if st.ForEach.OnItemFailure == "skip" {
		output["skipped"] = len(failures)
		output["failures"] = failures
	}
	if memoHits > 0 {
		output["memo_hits"] = memoHits
	}
	if passthroughHits > 0 {
		output["passthrough_hits"] = passthroughHits
	}
	return &HandlerResult{
		Output:      output,
		Usage:       usage,
		Memo:        memoHits == len(list) && len(list) > 0,
		Passthrough: passthroughHits == len(list) && len(list) > 0,
	}, nil
}

// withRetries drives attempts for retryable error classes.
func (e *Engine) withRetries(ctx context.Context, st *machine.State, runID string, fn func(attempt int) (*HandlerResult, error)) (*HandlerResult, error) {
	attemptsByClass := map[string]int{}
	attempt := 1
	for {
		res, err := fn(attempt)
		if err == nil {
			res.Attempts = attempt
			return res, nil
		}
		class := provider.Classify(err)
		attemptsByClass[class]++
		e.Listener.HandlerFailed(st.Name, class, err, attemptsByClass[class])
		_ = e.append(ctx, runID, journal.HandlerFailed, map[string]any{
			"state": st.Name, "class": class, "error": err.Error(), "attempt": attemptsByClass[class],
		})

		var policy *machine.RetryPolicy
		for i := range st.Retry {
			if st.Retry[i].Matches(class) {
				policy = &st.Retry[i]
				break
			}
		}
		if policy == nil || attemptsByClass[class] >= policy.MaxAttempts {
			return nil, err
		}

		delay := backoffDelay(policy.Backoff, attemptsByClass[class])
		e.Listener.RetryScheduled(st.Name, class, attemptsByClass[class]+1, delay)
		_ = e.append(ctx, runID, journal.RetryScheduled, map[string]any{
			"state": st.Name, "class": class, "attempt": attemptsByClass[class] + 1, "delay_ms": delay.Milliseconds(),
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		attempt++
	}
}

func backoffDelay(b machine.Backoff, attempt int) time.Duration {
	if b.Initial == 0 {
		return 0
	}
	d := b.Delay(attempt)
	if b.Jitter {
		d = time.Duration(float64(d) * (0.5 + mrand.Float64()/2))
	}
	if d < 0 || d > time.Duration(math.MaxInt64/2) {
		d = b.Cap
	}
	return d
}

func (e *Engine) append(ctx context.Context, runID string, t journal.EventType, data map[string]any) error {
	_, err := e.Store.Append(ctx, &journal.Event{RunID: runID, Type: t, Data: data})
	return err
}

func newRunID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x%s", time.Now().UnixMilli()&0xFFFFF, hex.EncodeToString(b))
}
