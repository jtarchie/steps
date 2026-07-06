package journal

import (
	"fmt"
	"time"
)

// RunState is the fold of a run's journal — everything the engine needs to
// continue a run from where the journal left off.
type RunState struct {
	Ctx         map[string]any       // run input at root + ctx.<state> = output
	Visits      map[string]int       // state entry counts
	Transitions int                  // transitions fired so far
	Usage       Usage                // cumulative
	Current     string               // current state name
	Started     time.Time            // run_started time (run timeout baseline)
	Parked      *ParkInfo            // non-nil while parked at a human gate
	Queued      bool                 // enqueued but not yet dispatched (only a run_enqueued event)
	Finished    bool                 // run reached a terminal
	Status      string               // final status when Finished
	Convos      map[string][]Message // last execution's conversation per state
	// Forks maps a fork's "state#visit" key to its pinned branch children, so a
	// resumed fork reattaches to the same child runs instead of spawning new
	// ones. Populated from fork_started; it does not affect the single cursor.
	Forks map[string][]ChildRef

	// Pending resume: set when the journal ends in run_resumed — the engine
	// consumes these as the parked state's handler result.
	ResumeEvent string
	ResumeData  map[string]any

	// InFlight: the current state was entered but its handler never finished
	// (crash mid-state). Resume re-runs the handler without re-counting the
	// visit.
	InFlight bool
}

// ParkInfo describes why and where a run is parked.
type ParkInfo struct {
	State     string        `json:"state"`
	Reason    string        `json:"reason"`
	Prompt    string        `json:"prompt"`
	At        time.Time     `json:"at"`
	Timeout   time.Duration `json:"timeout"`
	OnTimeout string        `json:"on_timeout"`
	// Choices is the gate's answer surface, rendered at park time so a later
	// CLI resume or the webview can present it without re-evaluating the
	// machine. Nil on journals written before choices existed.
	Choices *ParkChoices `json:"choices,omitempty"`
}

// ParkChoices is the renderable answer surface of a parked gate. Single
// gates (confirm included) route each option to its own resume event; multi
// gates emit one Event with the selected values in the gate's output.
type ParkChoices struct {
	Kind    string       `json:"kind"`            // single | multi
	Event   string       `json:"event,omitempty"` // multi only
	Options []ParkOption `json:"options"`
	Min     int          `json:"min,omitempty"`
	Max     int          `json:"max,omitempty"`
}

// ParkOption is one presentable answer.
type ParkOption struct {
	Event string `json:"event,omitempty"` // single: the resume event fired
	Value string `json:"value,omitempty"` // multi: the selected value
	Label string `json:"label"`
}

// Expired reports whether a parked human gate has passed its timeout.
func (p *ParkInfo) Expired(now time.Time) bool {
	return p.Timeout > 0 && now.After(p.At.Add(p.Timeout))
}

// Fold rebuilds RunState from the journal. Side effects are never replayed:
// their results already live in handler_finished events.
func Fold(events []*Event) *RunState {
	rs := &RunState{
		Ctx:    map[string]any{},
		Visits: map[string]int{},
		Convos: map[string][]Message{},
		Forks:  map[string][]ChildRef{},
	}
	for _, ev := range events {
		// Any later event clears a pending resume; only a trailing
		// run_resumed means "not yet consumed".
		rs.ResumeEvent, rs.ResumeData = "", nil

		//exhaustive:ignore // handler_failed/retry_scheduled are attempt-audit
		// events inside an already-in-flight state; they never conclude it
		// (only handler_finished/transition_fired do), so they leave RunState
		// untouched — resume re-runs the handler from InFlight, as it should.
		switch ev.Type {
		case RunEnqueued:
			rs.applyRunEnqueued(ev)
		case RunStarted:
			rs.applyRunStarted(ev)
		case ForkStarted:
			rs.applyForkStarted(ev)
		case StateEntered:
			rs.applyStateEntered(ev)
		case HandlerFinished:
			rs.applyHandlerFinished(ev)
		case TransitionFired:
			rs.applyTransitionFired(ev)
		case RunParked:
			rs.applyRunParked(ev)
		case RunResumed:
			rs.applyRunResumed(ev)
		case RunFinished:
			rs.applyRunFinished(ev)
		}
	}
	return rs
}

// applyRunEnqueued seeds the run's inputs and initial state from a durable
// enqueue, but leaves Started zero: the timeout baseline begins at dispatch
// (applyRunStarted), so queue wait does not count against limits.timeout.
func (rs *RunState) applyRunEnqueued(ev *Event) {
	rs.Queued = true
	if input, ok := ev.Data["input"].(map[string]any); ok {
		for k, v := range input {
			rs.Ctx[k] = v
		}
	}
	if initial, ok := ev.Data["initial"].(string); ok {
		rs.Current = initial
	}
}

func (rs *RunState) applyRunStarted(ev *Event) {
	rs.Queued = false
	rs.Started = ev.Time
	if input, ok := ev.Data["input"].(map[string]any); ok {
		for k, v := range input {
			rs.Ctx[k] = v
		}
	}
	// Seed the entry point (mirrors applyRunEnqueued). A later state_entered /
	// transition_fired overrides it; a run whose journal is only run_started —
	// a fresh parallel branch child — starts at this initial, not the machine's.
	if initial, ok := ev.Data["initial"].(string); ok {
		rs.Current = initial
	}
}

// ForkKey identifies a fork by state and visit — a loop body that forks each
// iteration gets a fresh child set, keyed by the entry count at fork time.
func ForkKey(state string, visit int) string {
	return fmt.Sprintf("%s#%d", state, visit)
}

func (rs *RunState) applyForkStarted(ev *Event) {
	state, _ := ev.Data["state"].(string)
	visit := intField(ev.Data["visit"])
	var payload struct {
		Children []ChildRef `json:"children"`
	}
	err := DecodeData(ev, &payload)
	if err == nil {
		rs.Forks[ForkKey(state, visit)] = payload.Children
	}
}

// intField coerces a journal number field: int in-process, float64 after a
// JSON round-trip through the store.
func intField(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func (rs *RunState) applyStateEntered(ev *Event) {
	state, _ := ev.Data["state"].(string)
	rs.Current = state
	rs.Visits[state]++
	rs.InFlight = true
}

func (rs *RunState) applyHandlerFinished(ev *Event) {
	rs.InFlight = false
	state, _ := ev.Data["state"].(string)
	if out, ok := ev.Data["output"].(map[string]any); ok {
		rs.Ctx[state] = out
	}
	var payload struct {
		Messages []Message `json:"messages"`
		Usage    Usage     `json:"usage"`
	}
	err := DecodeData(ev, &payload)
	if err == nil {
		if len(payload.Messages) > 0 {
			rs.Convos[state] = payload.Messages
		}
		rs.Usage.Add(payload.Usage)
	}
}

func (rs *RunState) applyTransitionFired(ev *Event) {
	rs.InFlight = false
	// Hops out of implicit distill states don't count toward maxTransitions
	// — mirror the engine's accounting.
	if impl, _ := ev.Data["implicit"].(bool); !impl {
		rs.Transitions++
	}
	if to, ok := ev.Data["to"].(string); ok {
		rs.Current = to
	}
}

func (rs *RunState) applyRunParked(ev *Event) {
	var p ParkInfo
	err := DecodeData(ev, &p)
	if err == nil {
		p.At = ev.Time
		rs.Parked = &p
	}
}

func (rs *RunState) applyRunResumed(ev *Event) {
	rs.Parked = nil
	rs.ResumeEvent, _ = ev.Data["event"].(string)
	rs.ResumeData, _ = ev.Data["data"].(map[string]any)
}

func (rs *RunState) applyRunFinished(ev *Event) {
	rs.Finished = true
	rs.Status, _ = ev.Data["status"].(string)
	if ts, ok := ev.Data["terminal_state"].(string); ok {
		rs.Current = ts
	}
}
