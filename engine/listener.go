package engine

import (
	"time"

	"github.com/jtarchie/steps/journal"
)

// Listener receives execution events for human-readable logging. The journal
// is the durable record; the listener is the live narration.
type Listener interface {
	RunStarted(runID, machineName string, input map[string]any)
	StateEntered(state, kind string, visit int, model string)
	ForEachItem(state string, index, total int, item any)
	AgentMessage(state, role, text string)
	ToolCalled(state, tool string, args map[string]any)
	ToolResult(state, tool string, result map[string]any)
	ToolRejected(state, tool, reason, mode string)
	HandlerFinished(state string, output map[string]any, event string, usage journal.Usage)
	HandlerFailed(state, class string, err error, attempt int)
	RetryScheduled(state, class string, attempt int, delay time.Duration)
	TransitionFired(from, to, on, when string)
	RunParked(runID, state, prompt string, timeout time.Duration)
	RunResumed(runID, event string)
	RunFinished(runID, status, terminal string, transitions int, usage journal.Usage)
	Warn(msg string, args ...any)
}

// NopListener discards everything (library embedding, tests).
type NopListener struct{}

func (NopListener) RunStarted(string, string, map[string]any)                       {}
func (NopListener) StateEntered(string, string, int, string)                        {}
func (NopListener) ForEachItem(string, int, int, any)                               {}
func (NopListener) AgentMessage(string, string, string)                             {}
func (NopListener) ToolCalled(string, string, map[string]any)                       {}
func (NopListener) ToolResult(string, string, map[string]any)                       {}
func (NopListener) ToolRejected(string, string, string, string)                     {}
func (NopListener) HandlerFinished(string, map[string]any, string, journal.Usage)   {}
func (NopListener) HandlerFailed(string, string, error, int)                        {}
func (NopListener) RetryScheduled(string, string, int, time.Duration)               {}
func (NopListener) TransitionFired(string, string, string, string)                  {}
func (NopListener) RunParked(string, string, string, time.Duration)                 {}
func (NopListener) RunResumed(string, string)                                       {}
func (NopListener) RunFinished(string, string, string, int, journal.Usage)          {}
func (NopListener) Warn(string, ...any)                                             {}
