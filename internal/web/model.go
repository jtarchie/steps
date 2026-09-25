package web

// Turning stored rows into what a page shows. The run transcript itself is
// folded by internal/runview, shared with the terminal; what is here is the
// web's own additions on top of it — spend, placements, the job board.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/runview"
	"github.com/jtarchie/steps/internal/store"
)

// The fold's types under the names this package has always used for them.
type (
	stepView   = runview.Step
	stepChange = runview.Change
	runFolder  = runview.Folder
)

// runView is a whole run, assembled.
type runView struct {
	runview.Transcript
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
	// Usage is what this run's agent steps spent, in step order, each row drawn on the step it joins to (StepUsage).
	Usage []store.AgentUsage
	// Placements is what the machines this run's placed steps ran on said about themselves, each drawn on the step it joins to (StepPlacements).
	//
	// ponytail: a usage or placement row that joins to no step in the transcript is not drawn anywhere. Every step publishes its start before it can be placed, so none is expected; if one turns up, draw the leftovers beside the run's own notes.
	Placements []store.Placement
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

// HasSpend keeps the head's spend total off a run that never called a model.
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
// that failed, and the page drew it verbatim beside a failed run.
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

// StepUsage is what this step spent, drawn on its own row.
func (r runView) StepUsage(step *stepView) []usageView {
	var rows []usageView

	for _, spent := range r.Usage {
		if !r.owns(step, spent.StepIndex, spent.StepName, spent.NodeHash) {
			continue
		}

		ceiling, known := r.ceilingFor(spent.StepName)
		rows = append(rows, usageView{AgentUsage: spent, StepFailed: step.Failed(), Ceiling: ceiling, CeilingKnown: known})
	}

	return rows
}

// StepPlacements is the machine this step ran on, drawn on its own row.
func (r runView) StepPlacements(step *stepView) []PlacementView {
	var rows []PlacementView

	for _, placed := range r.Placements {
		if r.owns(step, placed.StepIndex, placed.StepName, placed.NodeHash) {
			rows = append(rows, PlacementView{Placement: placed})
		}
	}

	return rows
}

// owns joins an agent_usage or run_placements row to the step it describes. Neither table records a step id, and (index, name) is not a step: every cell of an across:, member of an ensemble:, branch of an in_parallel: or race:, and the step a try: wraps is handed its block's index, and two may share a name. The node is what tells them apart — a step that ENDED WELL publishes the hash its row recorded — so a hashed step owns exactly its node's rows, and a step with no hash (it failed, or it is a hook, which is never hashed) owns the rows under its index and name that no hashed step claimed.
func (r runView) owns(step *stepView, index int, name, nodeHash string) bool {
	if step.Hash != "" {
		return nodeHash == step.Hash
	}

	if step.Skipped() || step.Index != index || step.Name != name {
		return false
	}

	for _, other := range r.Steps {
		if other.Hash != "" && other.Hash == nodeHash {
			return false
		}
	}

	return true
}

// StepHasBody is runview's answer plus what this page joins onto the row: a placed step with no output still has its machine to show.
func (r runView) StepHasBody(step *stepView) bool {
	return step.HasBody() || len(r.StepUsage(step)) > 0 || len(r.StepPlacements(step)) > 0
}

// StepHasDetail is runview's HasDetail over StepHasBody.
func (r runView) StepHasDetail(step *stepView) bool {
	return step.Container() || r.StepHasBody(step)
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

// Received is what came back from it: the tree the step produced there. A
// worker keeps what it produces too, so this is the cost of a LOCAL reader
// wanting the bytes, not of the step having made them.
func (p PlacementView) Received() string { return FormatBinaryBytes(p.BytesReceived) }

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

// mcpBlame finds the mcp server an error names. Two spellings, because internal/mcp writes two: a credential refusal names the server directly, and a dial failure names what it was connecting TO. Anything else is not an mcp failure and must not be decorated as one — a page that offers the same suggestion under every red run is one nobody reads.
var mcpBlame = regexp.MustCompile(`mcp(?:: connect to| server) "([^"]+)"`)

// MCPBlamed is the mcp server this run died on, or "" for a run that died of anything else. It exists because the reader asking whether a server is wired up arrives from a RED RUN, and until this link the answer lived on a tab they had to already know about.
func (r runView) MCPBlamed() string {
	found := mcpBlame.FindStringSubmatch(r.JobError)
	if found == nil {
		return ""
	}

	return found[1]
}

// HeadError is the run's error when no failed step holds it: a failing step's error is what the job error IS (the plan wraps it and returns it), so it is read on the step and the head only names that step — but a run that died before any step failed (a pull, a placement, a resource check) has no row to carry it.
func (r runView) HeadError() string {
	for _, step := range r.Steps {
		if step.Error != "" && strings.Contains(r.JobError, step.Error) {
			return ""
		}
	}

	return r.JobError
}

// MCPBlamedOn reports whether the mcp hint belongs under this step's error: the innermost failure, when it is the error the run died of.
func (r runView) MCPBlamedOn(step *stepView) bool {
	if r.MCPBlamed() == "" || step.Error == "" || !strings.Contains(r.JobError, step.Error) {
		return false
	}

	inner := r.InnermostFailure()

	return inner != nil && inner.Key() == step.Key()
}

// buildRunView folds a run's ordered events into steps. See runview.Build.
func buildRunView(run store.RunRow, rows []store.RunEventRow, results map[string]store.NodeRow) runView {
	return runView{Transcript: runview.Build(run, rows, results)}
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
	Name   string
	Latest store.RunRow
	HasRun bool
	// Held is the circuit breaker's state (max_consecutive_failures), never a person's pause.
	Held     bool
	Failures int
	HeldAt   string
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
		view.Held, view.Failures, view.HeldAt = true, breaker.Consecutive, breaker.PausedAt
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
