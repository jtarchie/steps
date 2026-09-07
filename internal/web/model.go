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
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// stepView is one step of a run, with whatever the step produced beneath it —
// including, for a block step, the steps that ran inside it.
type stepView struct {
	// ID and ParentID are the run's display tree (see events.Event). Zero on
	// a run recorded before the tree existed, which folds back to the flat
	// list this page used to be.
	ID       int64
	ParentID int64
	// Children are the steps that ran inside this one, in start order.
	Children []*stepView
	// FirstSeq is the sequence of the event that PUT this step on the page,
	// which is what the live stream asks when deciding whether the reader
	// already has the row. Not always a step.started: a step swallowed by a
	// chain skip is opened by its step.skipped, and asking about starts alone
	// meant such a row was swapped over an id that was never drawn — an
	// out-of-band swap htmx drops in silence.
	FirstSeq int64
	Index    int
	Name     string
	Kind     string
	Status   string
	Hash     string
	Reason   string // why it was skipped, when it was
	Error    string
	// Worker is where the step's commands ran, "tag (address)", empty for
	// this machine. Drawn because a red step on a fleet otherwise names no
	// machine — see events.Event.Worker.
	Worker   string
	Duration time.Duration
	Started  time.Time
	// Deadline is when a running agent step's resolved timeout: expires —
	// Started plus the ceiling — set by attachStepDeadlines (overview.go).
	// Zero (HasDeadline false) for a finished or non-agent step, an
	// unlimited timeout, or a run whose configuration no longer matches
	// what is loaded — the same "unknowable, not uncapped" reasoning
	// runView.Ceilings documents.
	Deadline time.Time
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

// Elapsed is how long a running step has been running, for the row's own
// clock. The page's timer script keeps it counting; this is what it reads
// before the first tick, and what a reader with scripting off sees.
func (s stepView) Elapsed() time.Duration {
	if s.Started.IsZero() {
		return 0
	}

	return time.Since(s.Started)
}

// HasDeadline reports whether Deadline could be resolved for this step.
func (s stepView) HasDeadline() bool { return !s.Deadline.IsZero() }

// Remaining is how long until Deadline, for the row's own clock — the
// countdown counterpart to Elapsed, floored at 0 rather than going negative
// for the rare page load that lands after the deadline technically passed
// but before the step's own end event has arrived.
func (s stepView) Remaining() time.Duration {
	if s.Deadline.IsZero() {
		return 0
	}

	if remaining := time.Until(s.Deadline); remaining > 0 {
		return remaining
	}

	return 0
}

// Skipped reports a step that did not execute.
func (s stepView) Skipped() bool { return s.Status == "skipped" }

// Failed reports a step that ended badly, by any classification.
func (s stepView) Failed() bool {
	return s.Status == "failed" || s.Status == "errored" || s.Status == "aborted"
}

// Container reports a step that ran other steps inside it.
func (s stepView) Container() bool { return len(s.Children) > 0 }

// Active reports a step still running, or holding something that is.
//
// It is what lights the rail down the branch the work is actually on, so a
// reader who has folded half the page still knows where to look. Recursive
// rather than a flag set at fold time, because a container's own status stays
// running until every child has finished — the two answers agree, and this
// one needs no second pass to maintain.
func (s stepView) Active() bool {
	if !s.Running() {
		return false
	}

	if !s.Container() {
		return true
	}

	for _, child := range s.Children {
		if child.Active() {
			return true
		}
	}

	// A container whose children have all finished while it has not is
	// between its last child and its own finish event. Nothing is running
	// inside it, so nothing about it should read as running.
	return false
}

// rollup counts how a container's subtree came out, for the row itself. A
// folded block still has to answer "where does this stand", and the rows that
// would otherwise answer are folded away with it.
type rollup struct {
	Cells   int
	Passed  int
	Failed  int
	Running int
	Skipped int
}

// Empty reports a rollup with nothing to say, which is not rendered.
//
// A container holding ONE step says nothing a reader cannot read off that
// step's own row — a try: wrapping a task would otherwise carry a permanent
// "1 step · 1 passed" that is pure furniture. Unless something inside went
// wrong: a failure has to survive the fold, however few steps it took.
func (r rollup) Empty() bool { return r.Cells == 0 || (r.Cells == 1 && r.Failed == 0) }

// Rollup summarises the step's DIRECT children — the unit a reader counts.
// A matrix reports its cells, not the tasks and agents inside them, which is
// the number the pipeline itself printed when it fanned out.
func (s stepView) Rollup() rollup {
	var out rollup

	for _, child := range s.Children {
		out.Cells++

		switch {
		case child.Skipped():
			out.Skipped++
		case child.Failed():
			out.Failed++
		case child.Running():
			out.Running++
		default:
			out.Passed++
		}
	}

	return out
}

// HasBody reports whether the step produced anything to show under its own
// row — as distinct from the steps that ran INSIDE it, which are a subtree.
//
// The two are separate because the body is rendered at all only when there is
// one: an empty .stepbody still carries its padding, which on a container
// opened a visible gap between the block and the first step inside it.
func (s stepView) HasBody(jobError string) bool {
	return len(s.Turns) > 0 ||
		len(s.Trajectory()) > 0 ||
		len(s.Outputs) > 0 ||
		s.DistinctError(jobError) != "" ||
		s.Response() != "" ||
		s.Note() != "" ||
		s.Reason != ""
}

// HasDetail reports whether the step has anything to show when expanded.
// A step with no body must not be foldable: an expandable row that opens onto
// nothing reads as a broken page, and a chevron that promises detail there
// isn't is worse than no chevron.
func (s stepView) HasDetail(jobError string) bool {
	return s.Container() || s.HasBody(jobError)
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

// Anchor is the step's own id in the page, and the target of the # link
// beside its name.
//
// Built on the step occurrence rather than on (index, name), because those
// two are shared: a try: and the step inside it produced the SAME anchor, so
// the page carried duplicate ids and a link pasted at someone opened the
// wrapper instead of the step they meant. The slug stays for a human reading
// the URL; the id is what makes it point at one row.
func (s stepView) Anchor() string {
	if s.ID == 0 {
		return fmt.Sprintf("step-%d-%s", s.Index, slugify(s.Name))
	}

	return fmt.Sprintf("step-%d-%s", s.ID, slugify(s.Name))
}

// Key identifies this row to the live stream, which must find the row the
// server already drew rather than appending a second one. Mirrors stepKey.
func (s stepView) Key() string {
	if s.ID != 0 {
		return "#" + strconv.FormatInt(s.ID, 10)
	}

	return fmt.Sprintf("%d/%s", s.Index, s.Name)
}

// callView is one recorded tool call read back from a node's result.
type callView struct {
	Name     string
	OK       bool
	ArgsJSON string
}

// Trajectory is the tool calls a step's node recorded, for the steps whose
// conversation never reached the event bus.
//
// A CLI-backed agent (source.model: "@claude/sonnet") owns its own tool loop
// in a subprocess, so it publishes no turns — but the calls it made are
// parsed out of the CLI's stream and stored in the node's result either way.
// The page showed the final answer and nothing about how it got there, while
// the record of exactly that sat one field away in a map it had already
// decoded.
//
// Empty when the step DID publish turns: those are the same calls, live and
// in order, and rendering both would show every tool call twice.
func (s stepView) Trajectory() []callView {
	if len(s.Turns) > 0 || s.Result == nil {
		return nil
	}

	recorded, _ := s.Result["trajectory"].([]any)

	calls := make([]callView, 0, len(recorded))

	for _, entry := range recorded {
		call, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		name, _ := call["name"].(string)
		if name == "" {
			continue
		}

		// A call whose "ok" is absent reads as having run, matching how the
		// CLI stream records one it never saw a result block for.
		succeeded, present := call["ok"].(bool)

		calls = append(calls, callView{
			Name:     name,
			OK:       succeeded || !present,
			ArgsJSON: encodeArgs(call["args"]),
		})
	}

	return calls
}

// encodeArgs renders a recorded call's arguments back to JSON, so they render
// through the same jsonValue path a live tool call's do.
func encodeArgs(args any) string {
	if args == nil {
		return ""
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}

	return string(encoded)
}

// Verdict pulls the routing verdict out of an agent step's result.
func (s stepView) Verdict() string { return s.resultString("verdict") }

// Note pulls the verdict's note.
func (s stepView) Note() string { return s.resultString("note") }

// WrappedUp reports a step whose conversation ran out of turns and was asked
// to answer from what it had already gathered.
//
// Worth its own marker for the reason the runner records it at all: the
// answer is degraded, and afterwards it is indistinguishable from a confident
// one. It is the counterpart of Truncated() on the spend panel — that one is
// the model's output being cut off mid-sentence, this one is the step's turn
// budget running out — and reading them together is how an author tells "the
// model had nothing more to say" from "the model was stopped".
func (s stepView) WrappedUp() bool {
	if s.Result == nil {
		return false
	}

	wrapped, _ := s.Result["wrapped_up"].(bool)

	return wrapped
}

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
	Run store.RunRow
	// Steps is every step of the run in start order, flat. The page renders
	// Roots instead; this is what the run-level questions (what changed, what
	// was cached) are still asked of, because they are about the run and not
	// about its shape.
	Steps []*stepView
	// Roots are the steps at the top of the plan, each holding its subtree.
	Roots    []*stepView
	JobError string
	// Changed names the steps whose content hash differs from the last
	// successful run of the same job — the "what is different this time"
	// answer a failed run opens with. Empty when there is no prior success
	// to compare against.
	Changed []string
	// ComparedTo is the run Changed was computed against.
	ComparedTo string
	// ComparedConfig is the configuration THAT run executed, when it is not
	// the one this run executed. Empty when the two agree, because a line
	// saying the configuration changed after every run says nothing — the
	// question it answers is whether a job that started behaving differently
	// was given different instructions.
	ComparedConfig string
	// Ceilings is what each agent step was ALLOWED to spend, by step name,
	// empty when this run's configuration is no longer the one being served.
	//
	// Spend with nothing to read it against answers no question — $2.83 of
	// unlimited and $2.83 of $3 are the same number — and this is the page a
	// reader is standing on when a step dies against a ceiling.
	//
	// Gated on the sha rather than resolved from the run's OWN revision, which
	// would be the better answer and is not available: a revision stores its
	// source but not its include files, and internal/config loads from a path
	// only, so a run older than the last edit cannot be reconstructed. Showing
	// today's ceiling for it would be wrong in exactly the case the column is
	// consulted for, so it is withheld and said to be withheld.
	//
	// ponytail: the sha gate. The upgrade is recording the ceilings the step
	// ran under on its agent_usage row at the moment it spends them (they are
	// resolved and in hand in saveAgentUsage), which needs a schemaVersion
	// bump and deletes agentCeilings, ceilingFor, ConfigDrifted and both the
	// "config changed" and "unknown" states.
	Ceilings map[string]string
	// ConfigDrifted is why Ceilings is empty: this run opened against a
	// configuration that is no longer loaded.
	ConfigDrifted bool
	// Usage is what this run's agent steps spent, in step order. Empty for a
	// run with no agent steps, which is what keeps the panel off a page that
	// has nothing to say about spend.
	Usage []store.AgentUsage
	// Placements is what the machines this run's placed steps ran on said
	// about themselves. Empty for a run with no placed steps, which keeps
	// the panel off every page of every pipeline that names no worker.
	Placements []store.Placement
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
	// USD is what the run cost, summed over the steps that reported a price.
	// Zero when nothing did, which is not the same as free — see Priced.
	USD float64
	// Unpriced is how many of those steps reported no price at all. A run
	// mixing a CLI agent with hosted ones has both kinds, and a total that
	// covers only some of its steps must SAY so — the same rule
	// store.RunCostTotals follows for the terminal report.
	Unpriced int
}

// Priced reports whether any step of this run reported a dollar figure.
//
// Only a CLI-backed agent does: it meters itself and prints the number when it
// exits, while every HTTP path reports tokens and leaves pricing to whoever
// knows the rate card. A run with none must say "unpriced" rather than
// "$0.00", which would read as free.
func (s spendSummary) Priced() bool { return s.USD > 0 }

// Cost renders the run's price, marked partial when some steps reported none —
// "$0.42+3?" is a bill for three of six steps, and presenting it as the whole
// one is the confidently-wrong number this column exists to avoid.
func (s spendSummary) Cost() string {
	rendered := FormatUSD(s.USD)
	if s.Unpriced > 0 {
		rendered += fmt.Sprintf("+%d?", s.Unpriced)
	}

	return rendered
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

		if step.CostUSD != nil {
			summary.USD += *step.CostUSD
		} else {
			summary.Unpriced++
		}

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
	// StepFailed is whether the step this spend belongs to ended failed,
	// which finish_reason cannot say. See FailedAfter.
	StepFailed bool
	// Ceiling is what this step was allowed to spend, rendered as the dials
	// table renders it, empty when the step's agent declared none.
	Ceiling string
	// CeilingKnown is whether Ceiling was resolved at all. An empty Ceiling
	// with this false is UNKNOWN, not uncapped — see runView.ceilingFor for
	// why the two must not share a spelling.
	CeilingKnown bool
}

// FailedAfter reports a step that failed after the request this row describes
// succeeded — the case where the finish column and the run contradict.
//
// finish_reason is the PROVIDER's word about the last request that completed,
// and for a cli agent it is the last INVOCATION's. A step whose first message
// finished cleanly and then died against a pooled ceiling — max_turns: and
// budget: usd do not reset at a message boundary — records "success" on a step
// that failed, and the panel drew it verbatim beside a failed run.
//
// Annotated rather than overwritten. The reason is a fact about a request;
// replacing it with steps' verdict on the step would put two vocabularies in
// one column and lose the only record of how the model actually stopped.
// Only when there IS a reason: a provider that reported nothing gets a row of
// zeros on purpose (see saveAgentUsage), and a step killed by timeout: is the
// common producer of one. Annotating that empty cell drew a dangling " — step
// failed" under a tooltip quoting a provider word that was never said.
func (u usageView) FailedAfter() bool {
	return u.StepFailed && u.FinishReason != "" &&
		!u.Truncated() && !strings.EqualFold(u.FinishReason, "error")
}

// Cost renders this step's price, empty when nothing reported one — a blank
// cell rather than a zero, for the same reason the header says "unpriced".
func (u usageView) Cost() string {
	if u.CostUSD == nil {
		return ""
	}

	return FormatUSD(*u.CostUSD)
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
	// Keyed by index AND name, because a plan index is not a step: every cell
	// of an across: and every member of an ensemble: is handed the block's own
	// index (internal/pipeline/across.go's runAcrossCell, ensemble.go's
	// runEnsembleMembers), so keying on it alone marked every sibling's spend
	// row failed when one cell failed. The name tells them apart — both sides
	// of this join spell it the same way, agent_usage from step.DisplayName()
	// and the event from eventStepName(), and both prefer a cell's Label.
	// The pair is still not a step: every member of an ensemble:, every
	// branch of an in_parallel: or race:, and the step a try: wraps are all
	// handed the block's index, and two of them may name the same agent. What
	// tells those apart is the node — a usage row always records its hash,
	// and a step that ENDED WELL publishes the same hash on its finish, while
	// a failed one publishes none. So a row whose node some step under this
	// key finished with is provably not the one that failed, and only the
	// rest are blamed.
	failed := make(map[string]bool, len(r.Steps))
	succeeded := make(map[string]bool, len(r.Steps))

	for _, step := range r.Steps {
		if step.Failed() {
			failed[usageKey(step.Index, step.Name)] = true
		} else if step.Hash != "" {
			succeeded[step.Hash] = true
		}
	}

	rows := make([]usageView, 0, len(r.Usage))

	for _, step := range r.Usage {
		ceiling, known := r.ceilingFor(step.StepName)
		rows = append(rows, usageView{
			AgentUsage:   step,
			StepFailed:   failed[usageKey(step.StepIndex, step.StepName)] && !succeeded[step.NodeHash],
			Ceiling:      ceiling,
			CeilingKnown: known,
		})
	}

	return rows
}

// usageKey identifies one executed step across the two tables that describe
// it. Not stepKey: agent_usage records no step id, so the pair is all there
// is to join on.
func usageKey(index int, name string) string {
	return strconv.Itoa(index) + "/" + name
}

// ceilingFor is the spend ceiling of the step this spend row belongs to, and
// whether it could be resolved at all.
//
// Ceilings are the AGENT's (see agentCeilings), and agent_usage records a STEP
// name. Those agree for an ordinary step and disagree for an across: cell,
// which renames itself to "<agent> [k=v]" (config.nameCell) while still
// resolving through the same agent — so the coordinates are dropped and the
// agent asked again.
//
// The bool is the part that matters, and it is why an uncapped agent is IN the
// map with an empty value rather than absent from it. nameCell returns early
// when a cell's name: template references an axis, leaving a name that matches
// neither the agent nor the "[k=v]" shape, so a miss is always reachable. A
// miss and a known-uncapped agent are opposite answers, and collapsing them
// prints "uncapped" for a step that had a ceiling — on the run where that
// ceiling is why it died, which is the one run this column exists for.
func (r runView) ceilingFor(stepName string) (string, bool) {
	return lookupByCellName(r.Ceilings, stepName)
}

// lookupByCellName resolves stepName against m, falling back to the base
// agent name when stepName carries an across: cell's "<agent> [k=v]" naming
// (see nameCell) — shared by ceilingFor and overview.go's timeoutForStep,
// which answer the same "which agent does this row belong to" question
// against two differently-typed per-agent maps (a spend ceiling, a resolved
// timeout), so the cell-name rule is defined once rather than risking the
// two drifting apart the next time either changes.
func lookupByCellName[T any](m map[string]T, stepName string) (T, bool) {
	if v, known := m[stepName]; known {
		return v, true
	}

	if at := strings.LastIndex(stepName, " ["); at > 0 && strings.HasSuffix(stepName, "]") {
		v, known := m[stepName[:at]]

		return v, known
	}

	var zero T

	return zero, false
}

// PlacementView is one placed step's machine as the template reads it.
//
// Exported because `steps runs where` renders the same rows through it. Two
// spellings of "which machine" had already drifted: the CLI's copy never
// learned Volatile, so the terminal — where an operator debugging a placed
// step looks first — reported a tmpfs workdir as an ordinary disk.
type PlacementView struct {
	store.Placement
}

// Platform is what the worker reported itself to be.
func (p PlacementView) Platform() string { return p.GOOS + "/" + p.GOARCH }

// Filesystem is what the tree landed on, or a stated silence.
//
// Empty is never drawn as an ordinary disk: a shim on a platform with no
// statfs genuinely cannot say, and tmpfs — the answer this column exists to
// surface — would otherwise hide behind a plausible blank.
func (p PlacementView) Filesystem() string {
	if p.FSType == "" {
		return "not reported"
	}

	return p.FSType + " (" + FormatBinaryBytes(p.FSFree) + " free)"
}

// Volatile marks a workdir that is MEMORY, so the row can say so in the
// colour every other warning on this page uses. It is the single most
// expensive thing a worker URL can get wrong and the least visible.
func (p PlacementView) Volatile() bool { return p.FSType == "tmpfs" || p.FSType == "ramfs" }

// Sent is what actually crossed to reach this machine. A worker keeps what it
// receives, so a step whose inputs were already there honestly reads 0 B.
func (p PlacementView) Sent() string { return FormatBinaryBytes(p.BytesSent) }

// Identity is who the step ran as, blank when the shim did not say — never an
// invented 0, which would read as root.
func (p PlacementView) Identity() string {
	if p.UID == nil || p.GID == nil {
		return ""
	}

	return strconv.Itoa(*p.UID) + ":" + strconv.Itoa(*p.GID)
}

// Machine names the host, and the image if the step ran in a container on it.
func (p PlacementView) Machine() string {
	if p.Image == "" {
		return p.Address
	}

	return p.Address + " in " + p.Image
}

// HasPlacements keeps the panel off a run that never left this machine.
func (r runView) HasPlacements() bool { return len(r.Placements) > 0 }

// PlacementRows wraps the raw rows for the template.
func (r runView) PlacementRows() []PlacementView {
	rows := make([]PlacementView, 0, len(r.Placements))
	for _, placed := range r.Placements {
		rows = append(rows, PlacementView{Placement: placed})
	}

	return rows
}

// FormatBinaryBytes renders a disk or transfer size in BINARY units,
// deliberately unlike formatBytes.
//
// That one is decimal so an agent's payload is comparable to the byte limits
// it is bounded by. This is about disks and wire transfers, which every tool
// a reader will cross-check against — the shim's own tmpfs warning, the EC2
// console, df — reports in KiB/MiB/GiB.
func FormatBinaryBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	units := [...]string{"KiB", "MiB", "GiB", "TiB"}

	// The step up happens where the RENDERED value would, not where the exact
	// one does: %.1f rounds, so anything from 1023.95 up prints as "1024.0" —
	// a size back over the unit boundary it was just divided under. One byte
	// short of a GiB read as 1024.0 MiB.
	size, exp := float64(n)/unit, 0
	for size >= unit-0.05 && exp < len(units)-1 {
		size, exp = size/unit, exp+1
	}

	return fmt.Sprintf("%.1f %s", size, units[exp])
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
	folder := newRunFolder()
	folder.add(rows, results)

	return folder.view(run)
}

// runFolder is the fold itself, kept open.
//
// The page reads a run once and folds every event in one go; the live stream
// folds each flush into the view it already has, which is the only way it can
// render a delta without re-reading the whole run 2.5 times a second — and
// without stopping dead at runEventLimit, which is what a re-read does to a
// run longer than that. Both go through this type rather than through two
// folds that would have to agree.
type runFolder struct {
	run   runView
	index map[string]int
}

func newRunFolder() *runFolder {
	return &runFolder{index: map[string]int{}}
}

// stepChange is what one batch of events did to a step's row, in the terms
// the live stream decides by: whether the row itself came or went, how many
// turns were hung under it, and whether anything else about it moved.
//
// Turns are counted rather than flagged because they are the one change the
// stream can send as an APPEND — the turn's own markup, not the row's — and
// only when nothing else about the row moved in the same batch. An agent step
// that spoke three hundred times used to be re-sent whole on each of them.
type stepChange struct {
	Opened bool
	Closed bool
	Other  bool
	Turns  int
}

// merge folds a later change on the same row into this one.
func (c stepChange) merge(other stepChange) stepChange {
	return stepChange{
		Opened: c.Opened || other.Opened,
		Closed: c.Closed || other.Closed,
		Other:  c.Other || other.Other,
		Turns:  c.Turns + other.Turns,
	}
}

func (c stepChange) any() bool { return c.Opened || c.Closed || c.Other || c.Turns > 0 }

// add folds one batch of events in, in order, reporting what changed on
// which steps' rows as a result.
//
// The fold reports it rather than the caller reading it off the events,
// because the two disagree exactly where it matters: a sub-agent's turns
// carry the CHILD's name, and attachTurn hangs them on the plan step still
// running. A caller deriving the set from stepKey(row) alone therefore holds
// a key belonging to no row — and the live stream, which sends only the rows
// its batch named, sent NOTHING for the whole of a sub-agent conversation.
func (f *runFolder) add(rows []store.RunEventRow, results map[string]store.NodeRow) map[string]stepChange {
	touched := make(map[string]stepChange, len(rows))

	for _, row := range rows {
		if row.Seq > f.run.LastSeq {
			f.run.LastSeq = row.Seq
		}

		if position, change := f.fold(row, results); change.any() {
			key := f.run.Steps[position].Key()
			touched[key] = touched[key].merge(change)
		}

		// The job's error is read by every row, not one: a step's own error
		// line is dropped once the job's error quotes it (DistinctError), and
		// with it, for a step that printed nothing else, the body and the
		// toggle. Those rows are re-drawn, or the reader keeps an error line a
		// reload no longer shows.
		if row.Type == events.TypeJobFinished && row.Text != "" {
			for _, step := range f.run.Steps {
				if step.Error != "" && strings.Contains(row.Text, step.Error) {
					touched[step.Key()] = touched[step.Key()].merge(stepChange{Other: true})
				}
			}
		}
	}

	return touched
}

// fold applies one event, reporting the position of the step whose row it
// changed — which is not always the step the event names — and how. See add.
func (f *runFolder) fold(row store.RunEventRow, results map[string]store.NodeRow) (int, stepChange) {
	switch row.Type {
	case events.TypeJobFinished:
		if row.Text != "" {
			f.run.JobError = row.Text
		}
	case events.TypeStepStarted:
		openStep(&f.run, f.index, row)

		return f.index[stepKey(row)], stepChange{Opened: true}
	case events.TypeStepFinished, events.TypeStepSkipped:
		if position, closed := closeStep(&f.run, f.index, row, results); closed {
			return position, stepChange{Closed: true}
		}
	case events.TypeStepOutput:
		attachOutput(&f.run, f.index, row)

		return f.index[stepKey(row)], stepChange{Other: true}
	default:
		// Agent conversation traffic; anything unrecognized is ignored
		// rather than rendered, so an event type added later cannot break
		// an older reader.
		if isAgentTraffic(row.Type) {
			if position, hung := attachTurn(&f.run, f.index, row); hung {
				return position, stepChange{Turns: 1}
			}
		}
	}

	return 0, stepChange{}
}

// view is what has been folded so far, with the tree hung and the run row as
// it stands — the row keeps changing under a live fold, and it is read for
// the job error the step template asks each row about. Safe to call after
// every batch: linkTree rebuilds the parent links rather than adding to them.
func (f *runFolder) view(run store.RunRow) runView {
	f.run.Run = run
	linkTree(&f.run)

	return f.run
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
	case events.TypeAgentSystem, events.TypeAgentUser,
		events.TypeAgentText, events.TypeAgentCall, events.TypeAgentResult, events.TypeAgentSubagent:
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
	view.Steps = append(view.Steps, &stepView{
		ID:       row.StepID,
		ParentID: row.ParentStepID,
		Index:    row.StepIndex,
		Name:     row.StepName,
		Kind:     row.StepKind,
		Started:  row.At,
		FirstSeq: row.Seq,
	})
}

// closeStep records how a step ended, folding in whatever its node recorded,
// and reports the position it wrote to.
func closeStep(
	view *runView, index map[string]int, row store.RunEventRow, results map[string]store.NodeRow,
) (int, bool) {
	position, seen := index[stepKey(row)]
	if !seen {
		// A step swallowed by a chain skip never started, so there is no row
		// to close — open one now. Dropping it instead would end the
		// transcript at the cache hit and leave every later step of the plan
		// unaccounted for, which reads as a truncated run rather than a
		// cached one.
		if row.Type != events.TypeStepSkipped {
			return 0, false
		}

		openStep(view, index, row)
		position = index[stepKey(row)]
	}

	step := view.Steps[position]
	step.Status = row.Status
	step.Hash = row.Hash
	step.Duration = time.Duration(row.DurationMS) * time.Millisecond

	// Only when set: a finished step's row carries it, and the events that
	// precede it do not, so an unconditional assignment would blank it.
	if row.Worker != "" {
		step.Worker = row.Worker
	}

	if row.Type == events.TypeStepSkipped {
		step.Reason = row.Text
	} else if step.Failed() {
		step.Error = row.Text
	}

	if node, ok := results[row.Hash]; ok && node.Result != "" {
		step.Result = decodeResult(node.Result)
	}

	return position, true
}

// attachTurn hangs one conversation event on the step it belongs to, and
// reports which step that was — which is not always the one the row names. A
// turn whose step is not in the transcript is dropped rather than inventing a
// step for it — that only happens for a hook or fix conversation, which by
// design records no plan step.
func attachTurn(view *runView, index map[string]int, row store.RunEventRow) (int, bool) {
	position, seen := index[stepKey(row)]
	if !seen {
		// A sub-agent's turns carry the CHILD's name, not the plan step's, so
		// fall back to the most recent agent step still running.
		position, seen = lastRunningAgent(view)
		if !seen {
			return 0, false
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

	return position, true
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

// stepKey identifies one step occurrence within a run.
//
// The minted id when there is one, because it is the only thing that tells a
// try: apart from the step it wraps — those publish the same index and the
// same name, and keying on that pair folded them into a single row that
// reported the wrapper's kind and the wrapped step's nothing.
//
// A run recorded before ids existed has none, and falls back to the pair.
// That run renders exactly as it did then: flat, with the collision it always
// had. Better than one row swallowing the whole plan, which is what keying
// every such step on id 0 would do.
func stepKey(row store.RunEventRow) string {
	if row.StepID != 0 {
		return "#" + strconv.FormatInt(row.StepID, 10)
	}

	return fmt.Sprintf("%d/%s", row.StepIndex, row.StepName)
}

// linkTree hangs each step under the container it named, leaving the steps
// with no container (or one this run never recorded) as roots.
//
// Order is start order throughout: the flat list is appended to as steps
// open, and a child is appended to its parent as it is linked, so a matrix's
// cells appear in the order they began rather than the order they finished.
func linkTree(view *runView) {
	// Rebuilt, not appended to: the live stream re-hangs the tree after every
	// flush, and appending would give a container a second copy of each of its
	// children per batch.
	view.Roots = view.Roots[:0]

	for _, step := range view.Steps {
		step.Children = step.Children[:0]
	}

	byID := make(map[int64]*stepView, len(view.Steps))

	for _, step := range view.Steps {
		if step.ID != 0 {
			byID[step.ID] = step
		}
	}

	for _, step := range view.Steps {
		parent, nested := byID[step.ParentID]
		// A step cannot contain itself, and a malformed pair must not build a
		// cycle the template would recurse through forever.
		if !nested || parent == step {
			view.Roots = append(view.Roots, step)

			continue
		}

		parent.Children = append(parent.Children, step)
	}
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

// stepCtx is what the recursive step template is invoked with: the page it is
// being drawn on, and the step to draw.
type stepCtx struct {
	Page map[string]any
	Step *stepView
	// OOB marks the ROOT of a fragment the live stream is sending, so it
	// carries the attribute that swaps it over the element already on the
	// page — the row for the `step` template, the head for `stephead`. Set
	// only on the outermost element: everything rendered under it is inside
	// that fragment, and htmx lifts any nested element carrying the attribute
	// OUT of the fragment to swap it on its own, which would leave the parent
	// morphed without it.
	OOB bool
}
