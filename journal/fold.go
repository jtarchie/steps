package journal

import "time"

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
	Finished    bool                 // run reached a terminal
	Status      string               // final status when Finished
	Convos      map[string][]Message // last execution's conversation per state

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
	At        time.Time     `json:"at"`
	Timeout   time.Duration `json:"timeout"`
	OnTimeout string        `json:"on_timeout"`
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
	}
	for _, ev := range events {
		// Any later event clears a pending resume; only a trailing
		// run_resumed means "not yet consumed".
		rs.ResumeEvent, rs.ResumeData = "", nil

		switch ev.Type {
		case RunStarted:
			rs.Started = ev.Time
			if input, ok := ev.Data["input"].(map[string]any); ok {
				for k, v := range input {
					rs.Ctx[k] = v
				}
			}
		case StateEntered:
			state, _ := ev.Data["state"].(string)
			rs.Current = state
			rs.Visits[state]++
			rs.InFlight = true
		case HandlerFinished:
			rs.InFlight = false
			state, _ := ev.Data["state"].(string)
			if out, ok := ev.Data["output"].(map[string]any); ok {
				rs.Ctx[state] = out
			}
			var payload struct {
				Messages []Message `json:"messages"`
				Usage    Usage     `json:"usage"`
			}
			if err := DecodeData(ev, &payload); err == nil {
				if len(payload.Messages) > 0 {
					rs.Convos[state] = payload.Messages
				}
				rs.Usage.Add(payload.Usage)
			}
		case TransitionFired:
			rs.InFlight = false
			rs.Transitions++
			if to, ok := ev.Data["to"].(string); ok {
				rs.Current = to
			}
		case RunParked:
			var p ParkInfo
			if err := DecodeData(ev, &p); err == nil {
				p.At = ev.Time
				rs.Parked = &p
			}
		case RunResumed:
			rs.Parked = nil
			rs.ResumeEvent, _ = ev.Data["event"].(string)
			rs.ResumeData, _ = ev.Data["data"].(map[string]any)
		case RunFinished:
			rs.Finished = true
			rs.Status, _ = ev.Data["status"].(string)
			if ts, ok := ev.Data["terminal_state"].(string); ok {
				rs.Current = ts
			}
		}
	}
	return rs
}
