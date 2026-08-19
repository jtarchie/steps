package web

// Turning stored rows into what a page shows.
//
// The run transcript is the load-bearing one: a run's events arrive as a flat
// ordered log (because that is what actually happened, including concurrent
// steps interleaving), and a reader needs them as steps with their traffic
// underneath. Everything here does that reshaping and nothing else — no
// queries, no HTTP — so the shapes are testable without a server.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// stepView is one step of a run, with whatever the step produced beneath it.
type stepView struct {
	Index    int
	Name     string
	Kind     string
	Status   string
	Hash     string
	Reason   string // why it was skipped, when it was
	Error    string
	Duration time.Duration
	Started  time.Time
	// Turns is agent conversation traffic that arrived while this step was
	// the one running. Empty for every other kind.
	Turns []turnView
	// Result is the node's recorded result, decoded — the verdict, the
	// response, the trajectory. Nil when the step recorded none.
	Result map[string]any
	// Outputs is what the step printed, one entry per output event.
	//
	// A slice rather than a string because a step with attempts: publishes one
	// output per attempt, and the live stream appends a block for each. Held as
	// a single overwritten string, the page a reader watched three attempts on
	// would silently drop two of them at the closing reload.
	Outputs []string
}

// Running reports a step that started and has not reported an end.
func (s stepView) Running() bool { return s.Status == "" || s.Status == "running" }

// Skipped reports a step that did not execute.
func (s stepView) Skipped() bool { return s.Status == "skipped" }

// Failed reports a step that ended badly, by any classification.
func (s stepView) Failed() bool {
	return s.Status == "failed" || s.Status == "errored" || s.Status == "aborted"
}

// HasDetail reports whether the step has anything to show when expanded.
// A step with no body must not be foldable: an expandable row that opens onto
// nothing reads as a broken page, and a chevron that promises detail there
// isn't is worse than no chevron.
func (s stepView) HasDetail(jobError string) bool {
	return len(s.Turns) > 0 ||
		len(s.Outputs) > 0 ||
		s.DistinctError(jobError) != "" ||
		s.Response() != "" ||
		s.Note() != "" ||
		s.Reason != ""
}

// DistinctError is the step's error, or "" when it is the same text the run
// already leads with. A failing step's error is usually what the job error IS
// (the plan wraps it and returns it), and printing one long message twice on
// the page a reader reaches while triaging is exactly where noise costs most.
func (s stepView) DistinctError(jobError string) string {
	if s.Error == "" || strings.Contains(jobError, s.Error) {
		return ""
	}

	return s.Error
}

// Verdict pulls the routing verdict out of an agent step's result.
func (s stepView) Verdict() string { return s.resultString("verdict") }

// Note pulls the verdict's note.
func (s stepView) Note() string { return s.resultString("note") }

// Response pulls the agent's final answer.
func (s stepView) Response() string { return s.resultString("response") }

func (s stepView) resultString(key string) string {
	if s.Result == nil {
		return ""
	}

	value, _ := s.Result[key].(string)

	return value
}

// Conversation is the step's turns with the one redundant turn dropped: the
// model's last text is normally the answer the Response block already shows in
// full, and printing a whole response twice on one page — once mid-transcript,
// once labeled — is noise exactly where a reader is trying to find the answer.
//
// Only that turn, and only when it matches: a model's running commentary
// mid-conversation is not the answer, and a response the model never said in a
// text turn (a wrapped-up conversation, a verdict-only step) still has to
// appear.
func (s stepView) Conversation() []turnView {
	response := strings.TrimSpace(s.Response())
	if response == "" || len(s.Turns) == 0 {
		return s.Turns
	}

	// The last text, not the last turn: a model that emits text AND a tool call
	// in one message records the result after the text, so keying on the
	// trailing turn let the answer through twice.
	for i := len(s.Turns) - 1; i >= 0; i-- {
		if s.Turns[i].Type != "agent_text" {
			continue
		}

		if strings.TrimSpace(s.Turns[i].Text) != response {
			return s.Turns
		}

		return append(s.Turns[:i:i], s.Turns[i+1:]...)
	}

	return s.Turns
}

// turnView is one piece of agent conversation traffic.
type turnView struct {
	Type   string
	Text   string
	Name   string
	Detail string
	Depth  int
	At     time.Time
}

// Nested reports a turn belonging to a delegated sub-agent rather than the
// step's own conversation.
func (t turnView) Nested() bool { return t.Depth > 0 }

// runView is a whole run, assembled.
type runView struct {
	Run      store.RunRow
	Steps    []stepView
	JobError string
	// Changed names the steps whose content hash differs from the last
	// successful run of the same job — the "what is different this time"
	// answer a failed run opens with. Empty when there is no prior success
	// to compare against.
	Changed []string
	// ComparedTo is the run Changed was computed against.
	ComparedTo string
	// Usage is what this run's agent steps spent, in step order. Empty for a
	// run with no agent steps, which is what keeps the panel off a page that
	// has nothing to say about spend.
	Usage []store.AgentUsage
	// LastSeq is the highest event sequence this view already renders, and it
	// is what the live stream must resume AFTER.
	//
	// Without it the stream opened at ?after=0 and replayed events the page
	// had already drawn, appending a second copy of every turn and every line
	// of output a run had produced before the tab was opened — visible on
	// exactly the page a person opens while a run is in flight.
	LastSeq int64
}

// Spend rolls this run's agent usage up for the page header.
type spendSummary struct {
	Tokens    int
	Cached    int
	Steps     int
	Truncated int
}

// CachePercent is the share of tokens the provider served from cache.
//
// The number prompt caching reports about itself, and the only place the
// feature is observable at all: the requests carry their headers either way.
func (s spendSummary) CachePercent() int {
	if s.Tokens <= 0 {
		return 0
	}

	return s.Cached * 100 / s.Tokens
}

// Spend summarises what this run's agent steps cost.
func (r runView) Spend() spendSummary {
	var summary spendSummary

	for _, step := range r.Usage {
		summary.Tokens += step.Total
		summary.Cached += step.Cached
		summary.Steps++

		if truncatedFinish(step.FinishReason) {
			summary.Truncated++
		}
	}

	return summary
}

// HasSpend keeps the panel off a run that never called a model.
func (r runView) HasSpend() bool { return len(r.Usage) > 0 }

// truncatedFinish reports a response cut off by the model's output limit
// rather than by having finished.
//
// Worth singling out because it is indistinguishable from a short answer
// otherwise, and a truncated verdict or JSON body wastes every step
// downstream of it.
func truncatedFinish(reason string) bool {
	return strings.EqualFold(reason, "length") || strings.EqualFold(reason, "max_tokens")
}

// Truncated reports whether this step's last response was cut off.
func (u usageView) Truncated() bool { return truncatedFinish(u.FinishReason) }

// usageView is one agent step's spend as the template reads it.
type usageView struct {
	store.AgentUsage
}

// CachePercent is this step's own cache hit rate.
func (u usageView) CachePercent() int {
	if u.Total <= 0 {
		return 0
	}

	return u.Cached * 100 / u.Total
}

// UsageRows wraps the raw rows for the template.
func (r runView) UsageRows() []usageView {
	rows := make([]usageView, 0, len(r.Usage))
	for _, step := range r.Usage {
		rows = append(rows, usageView{AgentUsage: step})
	}

	return rows
}

// Running reports a run still in flight, which is what decides whether the
// page opens a live event stream.
func (r runView) Running() bool { return r.Run.Status == "running" }

// HasSkipped reports whether any step replayed from cache. The page explains
// folding only when there is something folded — an explanation of a mechanism
// the reader cannot see on the page is noise.
func (r runView) HasSkipped() bool {
	for _, step := range r.Steps {
		if step.Skipped() {
			return true
		}
	}

	return false
}

// buildRunView folds a run's ordered events into steps.
//
// Steps are keyed by (index, name) rather than index alone: an across: cell
// and its siblings all report their parent's plan index, and collapsing them
// onto it would render a fan-out as one flickering step instead of the
// several concurrent ones it is.
func buildRunView(run store.RunRow, rows []store.RunEventRow, results map[string]store.NodeRow) runView {
	view := runView{Run: run}
	index := map[string]int{}

	for _, row := range rows {
		if row.Seq > view.LastSeq {
			view.LastSeq = row.Seq
		}

		switch row.Type {
		case events.TypeJobFinished:
			if row.Text != "" {
				view.JobError = row.Text
			}
		case events.TypeStepStarted:
			openStep(&view, index, row)
		case events.TypeStepFinished, events.TypeStepSkipped:
			closeStep(&view, index, row, results)
		case events.TypeStepOutput:
			attachOutput(&view, index, row)
		default:
			// Agent conversation traffic; anything unrecognized is ignored
			// rather than rendered, so an event type added later cannot break
			// an older reader.
			if isAgentTraffic(row.Type) {
				attachTurn(&view, index, row)
			}
		}
	}

	return view
}

// attachOutput hangs one of a step's printed outputs on it. The event can
// arrive before the step finishes, so the step is opened if it is not on the
// list yet — the same tolerance closeStep has for a chain-skipped step.
func attachOutput(view *runView, index map[string]int, row store.RunEventRow) {
	position, seen := index[stepKey(row)]
	if !seen {
		openStep(view, index, row)
		position = index[stepKey(row)]
	}

	view.Steps[position].Outputs = append(view.Steps[position].Outputs, row.Text)
}

// isAgentTraffic reports conversation events, which hang under a step rather
// than being one.
func isAgentTraffic(eventType string) bool {
	switch eventType {
	case events.TypeAgentText, events.TypeAgentCall, events.TypeAgentResult, events.TypeAgentSubagent:
		return true
	default:
		return false
	}
}

// openStep records a step's start, ignoring a repeat (a step re-entered by a
// to: loop keeps its first position rather than appearing twice).
func openStep(view *runView, index map[string]int, row store.RunEventRow) {
	key := stepKey(row)
	if _, seen := index[key]; seen {
		return
	}

	index[key] = len(view.Steps)
	view.Steps = append(view.Steps, stepView{
		Index:   row.StepIndex,
		Name:    row.StepName,
		Kind:    row.StepKind,
		Started: row.At,
	})
}

// closeStep records how a step ended, folding in whatever its node recorded.
func closeStep(view *runView, index map[string]int, row store.RunEventRow, results map[string]store.NodeRow) {
	position, seen := index[stepKey(row)]
	if !seen {
		// A step swallowed by a chain skip never started, so there is no row
		// to close — open one now. Dropping it instead would end the
		// transcript at the cache hit and leave every later step of the plan
		// unaccounted for, which reads as a truncated run rather than a
		// cached one.
		if row.Type != events.TypeStepSkipped {
			return
		}

		openStep(view, index, row)
		position = index[stepKey(row)]
	}

	step := &view.Steps[position]
	step.Status = row.Status
	step.Hash = row.Hash
	step.Duration = time.Duration(row.DurationMS) * time.Millisecond

	if row.Type == events.TypeStepSkipped {
		step.Reason = row.Text
	} else if step.Failed() {
		step.Error = row.Text
	}

	if node, ok := results[row.Hash]; ok && node.Result != "" {
		step.Result = decodeResult(node.Result)
	}
}

// attachTurn hangs one conversation event on the step it belongs to. A turn
// whose step is not in the transcript is dropped rather than inventing a
// step for it — that only happens for a hook or fix conversation, which by
// design records no plan step.
func attachTurn(view *runView, index map[string]int, row store.RunEventRow) {
	position, seen := index[stepKey(row)]
	if !seen {
		// A sub-agent's turns carry the CHILD's name, not the plan step's, so
		// fall back to the most recent agent step still running.
		position, seen = lastRunningAgent(view)
		if !seen {
			return
		}
	}

	view.Steps[position].Turns = append(view.Steps[position].Turns, turnView{
		Type:   row.Type,
		Text:   row.Text,
		Name:   row.Name,
		Detail: row.Detail,
		Depth:  parseDepth(row.Status),
		At:     row.At,
	})
}

// lastRunningAgent finds the newest agent step that has not finished.
func lastRunningAgent(view *runView) (int, bool) {
	for i := len(view.Steps) - 1; i >= 0; i-- {
		if view.Steps[i].Kind == "agent" && view.Steps[i].Running() {
			return i, true
		}
	}

	return 0, false
}

// parseDepth reads the "depth:N" marker a nested conversation's events carry.
func parseDepth(status string) int {
	if !strings.HasPrefix(status, "depth:") {
		return 0
	}

	var depth int

	_, err := fmt.Sscanf(status, "depth:%d", &depth)
	if err != nil {
		return 0
	}

	return depth
}

// stepKey identifies a step within one run.
func stepKey(row store.RunEventRow) string {
	return fmt.Sprintf("%d/%s", row.StepIndex, row.StepName)
}

// decodeResult decodes a node's stored result JSON, yielding nil rather than
// failing: a result that will not parse is a diagnostic curiosity, not a
// reason for the page not to render.
func decodeResult(raw string) map[string]any {
	var result map[string]any

	err := json.Unmarshal([]byte(raw), &result)
	if err != nil {
		return nil
	}

	return result
}

// diffAgainst computes which steps differ, by content hash, between this run
// and a prior one. It is the merkle store answering "what is different about
// this run" directly: identical hashes mean identical content, so a step
// whose hash moved is a step whose inputs, command, or prompt moved.
func diffAgainst(current, prior runView) []string {
	priorHashes := map[string]string{}
	for _, step := range prior.Steps {
		priorHashes[step.Name] = step.Hash
	}

	var changed []string

	for _, step := range current.Steps {
		before, existed := priorHashes[step.Name]
		if !existed {
			changed = append(changed, step.Name+" (new)")

			continue
		}

		if before != step.Hash && step.Hash != "" && before != "" {
			changed = append(changed, step.Name)
		}
	}

	return changed
}

// jobView is a job as the board and the job page show it.
type jobView struct {
	Name     string
	Latest   store.RunRow
	HasRun   bool
	Paused   bool
	Failures int
	// Upstream and Downstream are the passed: constraint graph, per resource.
	Upstream   []edgeView
	Downstream []edgeView
}

// edgeView is one passed: dependency: a resource that must be green in some
// other job. The resource is the edge's identity, because that is what the
// constraint is actually about — a job does not depend on a job, a get
// depends on a version having passed somewhere.
type edgeView struct {
	Resource string
	Job      string
}

// buildJobViews assembles the board: every configured job, its latest run,
// its breaker state, and the dependency edges in both directions.
func buildJobViews(cfg *config.Config, latest map[string]store.RunRow, paused []store.PausedJob) []jobView {
	pausedBy := map[string]store.PausedJob{}
	for _, job := range paused {
		pausedBy[job.Name] = job
	}

	views := make([]jobView, 0, len(cfg.Jobs))

	for _, job := range cfg.Jobs {
		views = append(views, buildJobView(job, latest, pausedBy))
	}

	return linkDownstream(views)
}

// buildJobView assembles one job's row: its latest run, its breaker state, and
// the passed: constraints it declares.
func buildJobView(job config.Job, latest map[string]store.RunRow, pausedBy map[string]store.PausedJob) jobView {
	view := jobView{Name: job.Name}

	if run, ok := latest[job.Name]; ok {
		view.Latest, view.HasRun = run, true
	}

	if breaker, ok := pausedBy[job.Name]; ok {
		view.Paused, view.Failures = true, breaker.Consecutive
	}

	for resource, upstream := range job.PassedConstraints() {
		for _, name := range upstream {
			view.Upstream = append(view.Upstream, edgeView{Resource: resource, Job: name})
		}
	}

	sortEdges(view.Upstream)

	return view
}

// linkDownstream fills in each job's downstream edges by reading every
// upstream edge backwards. Derived rather than stored, so the two directions
// cannot disagree.
func linkDownstream(views []jobView) []jobView {
	byName := map[string]int{}
	for i, view := range views {
		byName[view.Name] = i
	}

	for _, view := range views {
		for _, edge := range view.Upstream {
			position, ok := byName[edge.Job]
			if !ok {
				continue
			}

			views[position].Downstream = append(views[position].Downstream,
				edgeView{Resource: edge.Resource, Job: view.Name})
		}
	}

	for i := range views {
		sortEdges(views[i].Downstream)
	}

	return views
}

func sortEdges(edges []edgeView) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Job == edges[j].Job {
			return edges[i].Resource < edges[j].Resource
		}

		return edges[i].Job < edges[j].Job
	})
}
