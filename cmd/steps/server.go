package main

// The webview is a read-mostly window on the journal plus a gate-answer
// form. It shares the SQLite file with any running `steps run` processes:
// WAL + busy_timeout(5000) (journal/sqlite.go) makes concurrent readers and
// short writes safe. The only writes here happen during a resume, guarded by
// the in-process `resuming` set so a double-POST can't launch two engines on
// one run. Cross-process double-resume is prevented by the gate itself — the
// second Resume folds a journal whose park is already consumed and errors.
// (Two engines appending to the SAME run would still race on MAX(seq)+1;
// "one resumer per parked gate" keeps that off the table in practice.)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
)

type server struct {
	store       journal.Store
	eng         *engine.Engine
	resuming    sync.Map             // runID -> struct{}: a resume is in flight
	hooksByPath map[string]*hookSpec // webhook path slug -> hook; nil = none registered
	disp        *dispatcher          // drains the durable queue; nil when no hooks
}

// hookSpec is a webhook-triggerable machine, loaded once at startup.
type hookSpec struct {
	m      *machine.Machine
	inputs map[string]any // --hook-input base values (operator config)
	token  string         // resolved per-hook token; "" = unauthenticated
}

// newServer wires the routes onto a fresh echo instance — the httptest seam.
// ctx bounds the dispatcher goroutine (cancel it to stop draining the queue).
func newServer(ctx context.Context, store journal.Store, eng *engine.Engine, hooks map[string]*hookSpec, maxInFlight int) *echo.Echo {
	s := &server{store: store, eng: eng, hooksByPath: hooks}
	if len(hooks) > 0 {
		s.disp = newDispatcher(eng, store, hooks, maxInFlight)
		go s.disp.run(ctx)
	}
	e := echo.New()
	e.GET("/", func(c *echo.Context) error { return c.Redirect(http.StatusFound, "/runs") })
	e.GET("/runs", s.handleRuns)
	e.GET("/runs/:id", s.handleRun)
	e.POST("/runs/:id/resume", s.handleResume)
	e.POST("/hooks/:path", s.handleHook)
	return e
}

// handleHook receives an inbound webhook, maps the JSON payload to run inputs
// via the machine's webhook.map, and durably enqueues a run. A dispatcher
// starts it when a slot frees; a full queue is rejected with 429. Trigger-only:
// gates are answered via the UI or CLI, never the webhook.
func (s *server) handleHook(c *echo.Context) error {
	ctx := c.Request().Context()
	hook := s.hooksByPath[c.Param("path")]
	if hook == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no such hook")
	}
	herr := checkHookToken(c, hook.token)
	if herr != nil {
		return herr
	}

	var body map[string]any
	err := json.NewDecoder(c.Request().Body).Decode(&body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "hook payload must be a JSON object: "+err.Error())
	}

	// One flat scope: the request, plus operator-supplied hook inputs by name.
	scope := map[string]any{"body": body, "headers": flatHeaders(c), "query": flatQuery(c)}
	for k, v := range hook.inputs {
		scope[k] = v
	}
	mapped, err := hook.m.Webhook.Map.Map(scope)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "webhook.map: "+err.Error())
	}

	input, herr := hookInput(hook, mapped)
	if herr != nil {
		return herr
	}

	// Backpressure: reject when the durable queue for this hook is full. Soft
	// cap — concurrent POSTs count-then-insert can overshoot by a few.
	herr = s.checkQueueDepth(ctx, hook)
	if herr != nil {
		return herr
	}

	runID, err := s.eng.Enqueue(ctx, hook.m, input, hook.m.Webhook.Path)
	if err != nil {
		return fmt.Errorf("enqueuing hook run: %w", err)
	}
	if s.disp != nil {
		s.disp.poke()
	}

	err = c.JSON(http.StatusAccepted, map[string]any{"machine": hook.m.Name, "run": runID, "status": "queued"})
	if err != nil {
		return fmt.Errorf("responding to hook: %w", err)
	}
	return nil
}

// hookMaxQueued resolves the hook's queue bound, applying the default.
func hookMaxQueued(hook *hookSpec) int {
	n := hook.m.Webhook.MaxQueued
	if n <= 0 {
		n = machine.DefaultHookMaxQueued
	}
	return n
}

// checkQueueDepth returns 429 when this hook already has maxQueued runs waiting
// for a slot.
func (s *server) checkQueueDepth(ctx context.Context, hook *hookSpec) *echo.HTTPError {
	limit := hookMaxQueued(hook)
	queued, err := s.store.ListRunsByStatus(ctx, journal.StatusQueued)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "checking queue depth: "+err.Error())
	}
	n := 0
	for _, r := range queued {
		if r.Machine == hook.m.Name {
			n++
		}
	}
	if n >= limit {
		return echo.NewHTTPError(http.StatusTooManyRequests,
			fmt.Sprintf("hook queue full (%d/%d) — retry later", n, limit))
	}
	return nil
}

// checkHookToken enforces the shared secret when one is configured, accepting
// it as a Bearer header or a ?token= query param.
func checkHookToken(c *echo.Context, token string) *echo.HTTPError {
	if token == "" {
		return nil
	}
	got := c.QueryParam("token")
	if auth := c.Request().Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		got = strings.TrimPrefix(auth, "Bearer ")
	}
	if got != token {
		return echo.NewHTTPError(http.StatusUnauthorized, "bad or missing hook token")
	}
	return nil
}

// hookInput merges hook inputs (under) with the map's payload-derived values
// (over), keeping only declared inputs, and fails if a required one is absent.
func hookInput(hook *hookSpec, mapped map[string]any) (map[string]any, *echo.HTTPError) {
	input := map[string]any{}
	for k, v := range hook.inputs {
		if _, ok := hook.m.Input[k]; ok {
			input[k] = v
		}
	}
	for k, v := range mapped {
		if _, ok := hook.m.Input[k]; ok {
			input[k] = v
		}
	}
	for name, spec := range hook.m.Input {
		if spec.Required {
			if _, ok := input[name]; !ok {
				return nil, echo.NewHTTPError(http.StatusBadRequest,
					fmt.Sprintf("payload did not produce required input %q", name))
			}
		}
	}
	return input, nil
}

func flatHeaders(c *echo.Context) map[string]any {
	out := map[string]any{}
	for k, vs := range c.Request().Header {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

func flatQuery(c *echo.Context) map[string]any {
	out := map[string]any{}
	for k, vs := range c.Request().URL.Query() {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// ---- runs list --------------------------------------------------------------

type runRow struct {
	*journal.Run
	Prompt string
	Tokens int
	Cost   float64
}

type machineTotals struct {
	Machine                    string
	Runs, Done, Failed, Queued int
	Tokens                     int
	Cost                       float64
}

func (s *server) handleRuns(c *echo.Context) error {
	ctx := c.Request().Context()
	filter := c.QueryParam("status")

	runs, err := s.store.ListRuns(ctx)
	if err != nil {
		return fmt.Errorf("listing runs: %w", err)
	}

	var rows []runRow
	var attention []runRow
	totalsByMachine := map[string]*machineTotals{}
	anyActive := false // running or queued -> auto-refresh

	for _, r := range runs {
		rs := s.fold(ctx, r.ID)
		row := runRow{Run: r}
		if rs != nil {
			row.Tokens = rs.Usage.Total()
			row.Cost = rs.Usage.Cost
			if rs.Parked != nil {
				row.Prompt = clipLine(rs.Parked.Prompt, 160)
			}
		}

		t := totalsByMachine[r.Machine]
		if t == nil {
			t = &machineTotals{Machine: r.Machine}
			totalsByMachine[r.Machine] = t
		}
		t.Runs++
		t.Tokens += row.Tokens
		t.Cost += row.Cost
		switch r.Status {
		case journal.StatusDone:
			t.Done++
		case journal.StatusFailed:
			t.Failed++
		case journal.StatusQueued:
			t.Queued++
			anyActive = true
		case journal.StatusRunning:
			anyActive = true
		}

		if r.Status == journal.StatusParked {
			attention = append(attention, row)
		}
		if filter == "" || r.Status == filter {
			rows = append(rows, row)
		}
	}

	totals := make([]machineTotals, 0, len(totalsByMachine))
	for _, t := range totalsByMachine {
		totals = append(totals, *t)
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i].Machine < totals[j].Machine })

	refresh := 0
	if anyActive {
		refresh = 5
	}
	return s.render(c, http.StatusOK, "runs.html", map[string]any{
		"Title":     "steps — runs",
		"Refresh":   refresh,
		"Filter":    filter,
		"Runs":      rows,
		"Attention": attention,
		"Totals":    totals,
	})
}

// ---- run detail -------------------------------------------------------------

type execRow struct {
	State    string
	Visit    int
	Event    string
	Memo     bool
	TokIn    int
	TokOut   int
	Duration string
	Hint     string
}

type failureRow struct {
	State string
	Class string
	Count int
}

type artifactRow struct {
	Path       string
	State      string
	Bytes      int
	Content    string
	HasContent bool
}

// stepFailure is one failed attempt within a state execution.
type stepFailure struct {
	Class   string
	Attempt int
	Error   string
}

// writtenFile is a file a step's output recorded writing (path + size only —
// the bytes themselves are not journaled).
type writtenFile struct {
	Path  string
	Bytes int
}

// timelineItem is one entry in the chronological run story. Kind selects
// which fields matter: "exec" (a state execution and everything it did),
// "transition" (a routing hop), or "park"/"resume" (gate markers).
type timelineItem struct {
	Kind string

	// exec
	State     string
	Visit     int
	StateKind string // agent | action | human | terminal (from the pinned machine)
	Model     string // agent steps only
	Event     string
	Memo      bool
	TokIn     int
	TokOut    int
	Duration  string
	Hint      string
	Messages  []journal.Message
	Output    map[string]any
	Wrote     []writtenFile
	Failures  []stepFailure

	// transition
	From, To, On, Guard string
	Implicit            bool

	// park / resume
	Prompt string
}

// stateInfo is the static shape of a state, recovered from the pinned
// machine so the timeline can label steps with their kind and model.
type stateInfo struct {
	Kind  string
	Model string
}

func (s *server) handleRun(c *echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	run, err := s.store.GetRun(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "run not found")
	}
	events, err := s.store.Events(ctx, id)
	if err != nil {
		return fmt.Errorf("loading journal for run %s: %w", id, err)
	}
	rs := journal.Fold(events)

	// Parse the pinned machine once: it labels the timeline (kind/model) and
	// draws the topology diagram. A parse failure is non-fatal — the page still
	// renders, just without those.
	m, merr := loadPinnedMachine(run)
	minfo := map[string]stateInfo{}
	if merr == nil {
		minfo = stateInfoFrom(m)
	}
	rows := execRows(events)
	failures := failureRows(events)
	artifacts := artifactRows(rs)
	timeline := buildTimeline(events, minfo)
	inputs := runInputs(events)
	// Parallel branch children (if any) surface here rather than in the runs
	// list; each is viewable as its own sub-run page.
	children, _ := s.store.ListChildRuns(ctx, id)

	hash := run.Hash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	refresh := 0
	if run.Status == journal.StatusRunning || run.Status == journal.StatusQueued {
		refresh = 3
	}

	data := map[string]any{
		"Title": "steps — " + run.ID,
		// The run page drives its own auto-refresh in JS (run.html) so it can
		// pause while the graph is being panned/zoomed; no <meta refresh> here.
		"Refresh":     0,
		"AutoRefresh": refresh,
		"Run":         run,
		"Hash":        hash,
		"Transitions": rs.Transitions,
		"Usage":       rs.Usage,
		"Rows":        rows,
		"Failures":    failures,
		"Artifacts":   artifacts,
		"Timeline":    timeline,
		"Inputs":      inputs,
		"Children":    children,
	}
	if merr == nil {
		data["Graph"] = renderGraphSVG(m.Graph(), buildRunOverlay(m, run, rs, events))
	}
	if p := rs.Parked; p != nil && run.Status == journal.StatusParked {
		data["Parked"] = p
		data["GateSingle"] = p.Choices != nil && p.Choices.Kind == "single" && len(p.Choices.Options) > 0
		data["GateMulti"] = p.Choices != nil && p.Choices.Kind == "multi"
		if p.Timeout > 0 {
			data["GateExpires"] = humanizeSince(p.At.Add(p.Timeout))
		}
	}
	return s.render(c, http.StatusOK, "run.html", data)
}

// execRows mirrors inspect.go's per-execution derivation, adding a duration
// (state_entered ts paired with the next terminating event for that state)
// and a one-glance result hint.
func execRows(events []*journal.Event) []execRow {
	var rows []execRow
	visitSeen := map[string]int{}
	enteredAt := map[string]time.Time{}

	for _, ev := range events {
		state, _ := ev.Data["state"].(string)
		//exhaustive:ignore // only cares about the events that shape a per-execution row
		switch ev.Type {
		case journal.StateEntered:
			enteredAt[state] = ev.Time
		case journal.HandlerFinished:
			var d struct {
				State  string         `json:"state"`
				Event  string         `json:"event"`
				Output map[string]any `json:"output"`
				Usage  journal.Usage  `json:"usage"`
				Memo   bool           `json:"memo"`
			}
			if journal.DecodeData(ev, &d) != nil {
				continue
			}
			visitSeen[d.State]++
			row := execRow{
				State:  d.State,
				Visit:  visitSeen[d.State],
				Event:  d.Event,
				Memo:   d.Memo,
				TokIn:  d.Usage.InputTokens,
				TokOut: d.Usage.OutputTokens,
				Hint:   hint(d.Output),
			}
			if t, ok := enteredAt[d.State]; ok {
				row.Duration = ev.Time.Sub(t).Round(100 * time.Millisecond).String()
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func failureRows(events []*journal.Event) []failureRow {
	counts := map[string]map[string]int{}
	for _, ev := range events {
		if ev.Type != journal.HandlerFailed {
			continue
		}
		state, _ := ev.Data["state"].(string)
		class, _ := ev.Data["class"].(string)
		if counts[state] == nil {
			counts[state] = map[string]int{}
		}
		counts[state][class]++
	}
	var out []failureRow
	states := make([]string, 0, len(counts))
	for st := range counts {
		states = append(states, st)
	}
	sort.Strings(states)
	for _, st := range states {
		for class, n := range counts[st] {
			out = append(out, failureRow{State: st, Class: class, Count: n})
		}
	}
	return out
}

// loadPinnedMachine re-evaluates the exact machine a run started with, from the
// source + assets pinned in its journal. Shared by the timeline labels and the
// topology diagram so the page parses it once.
func loadPinnedMachine(run *journal.Run) (*machine.Machine, error) {
	m, err := machine.ParseWithAssets(run.Source, run.Assets, parseOpts()...)
	if err != nil {
		return nil, fmt.Errorf("re-evaluating pinned machine: %w", err)
	}
	return m, nil
}

// stateInfoFrom recovers each state's kind and model so the timeline can label
// steps.
func stateInfoFrom(m *machine.Machine) map[string]stateInfo {
	out := map[string]stateInfo{}
	for _, st := range m.States {
		si := stateInfo{Kind: st.HandlerKind()}
		if st.Agent != nil {
			si.Model = st.Agent.Model.Display()
		}
		out[st.Name] = si
	}
	return out
}

// buildRunOverlay projects a run's journal onto the topology: which states were
// visited (and how often), which failed, which edges fired, and where the run
// currently sits. Journal events carry the machine's *lowered* names, so every
// name is mapped through machine.VisibleState — a distill hop (`consumer#key`)
// collapses onto the visible consumer, so a run that traversed `pred → C#key →
// C` lights the drawn `pred → C` edge and a mid-distill park/fail still marks a
// node. A consumer's visit count therefore also includes its distiller passes.
func buildRunOverlay(m *machine.Machine, run *journal.Run, rs *journal.RunState, events []*journal.Event) RunOverlay {
	vis := m.VisibleState

	visits := map[string]int{}
	for name, count := range rs.Visits {
		visits[vis(name)] += count
	}
	failed := map[string]bool{}
	for name := range failedSet(events) {
		failed[vis(name)] = true
	}
	fired := map[[2]string]bool{}
	for pair := range firedEdges(events) {
		from, to := vis(pair[0]), vis(pair[1])
		if from != to { // internal distill hops collapse to a self-pair; drop
			fired[[2]string{from, to}] = true
		}
	}
	return RunOverlay{
		Visits:  visits,
		Failed:  failed,
		Fired:   fired,
		Current: vis(run.CurrentState),
		Parked:  run.Status == journal.StatusParked,
		Status:  run.Status,
	}
}

// failedSet is the set of states that recorded a handler_failed.
func failedSet(events []*journal.Event) map[string]bool {
	out := map[string]bool{}
	for _, ev := range events {
		if ev.Type == journal.HandlerFailed {
			if s, _ := ev.Data["state"].(string); s != "" {
				out[s] = true
			}
		}
	}
	return out
}

// firedEdges is the set of {from,to} transitions the run actually took.
func firedEdges(events []*journal.Event) map[[2]string]bool {
	out := map[[2]string]bool{}
	for _, ev := range events {
		if ev.Type == journal.TransitionFired {
			from, _ := ev.Data["from"].(string)
			to, _ := ev.Data["to"].(string)
			if from != "" && to != "" {
				out[[2]string{from, to}] = true
			}
		}
	}
	return out
}

// runInputs returns the run's inputs as recorded in run_started, falling back
// to run_enqueued so a still-queued run (no run_started yet) shows its inputs.
func runInputs(events []*journal.Event) map[string]any {
	for _, ev := range events {
		if ev.Type == journal.RunStarted || ev.Type == journal.RunEnqueued {
			if in, ok := ev.Data["input"].(map[string]any); ok {
				return in
			}
			return nil
		}
	}
	return nil
}

// buildTimeline folds the journal into the chronological run story: each
// state execution (with its conversation, output, writes, and failures), the
// transitions between them, and gate park/resume markers — in journal order.
func buildTimeline(events []*journal.Event, minfo map[string]stateInfo) []timelineItem {
	var items []timelineItem
	enteredAt := map[string]time.Time{}
	visitSeen := map[string]int{}
	pending := -1 // index of the exec awaiting its handler_finished

	for _, ev := range events {
		state, _ := ev.Data["state"].(string)
		//exhaustive:ignore // only the events that shape the run story are rendered
		switch ev.Type {
		case journal.StateEntered:
			enteredAt[state] = ev.Time
			visitSeen[state]++
			items = append(items, timelineItem{
				Kind:      "exec",
				State:     state,
				Visit:     visitSeen[state],
				StateKind: minfo[state].Kind,
				Model:     minfo[state].Model,
			})
			pending = len(items) - 1
		case journal.HandlerFailed:
			if pending >= 0 {
				class, _ := ev.Data["class"].(string)
				msg, _ := ev.Data["error"].(string)
				attempt, _ := ev.Data["attempt"].(float64)
				items[pending].Failures = append(items[pending].Failures, stepFailure{
					Class: class, Attempt: int(attempt), Error: msg,
				})
			}
		case journal.HandlerFinished:
			var d struct {
				State    string            `json:"state"`
				Event    string            `json:"event"`
				Output   map[string]any    `json:"output"`
				Usage    journal.Usage     `json:"usage"`
				Messages []journal.Message `json:"messages"`
				Memo     bool              `json:"memo"`
			}
			if journal.DecodeData(ev, &d) != nil {
				continue
			}
			if pending < 0 || items[pending].State != d.State {
				continue // finish without a matching entry — shouldn't happen
			}
			it := &items[pending]
			it.Event = d.Event
			it.Memo = d.Memo
			it.TokIn = d.Usage.InputTokens
			it.TokOut = d.Usage.OutputTokens
			it.Hint = hint(d.Output)
			it.Messages = d.Messages
			it.Output = d.Output
			it.Wrote = writtenFiles(d.Output)
			if t, ok := enteredAt[d.State]; ok {
				it.Duration = ev.Time.Sub(t).Round(100 * time.Millisecond).String()
			}
			pending = -1
		case journal.TransitionFired:
			from, _ := ev.Data["from"].(string)
			to, _ := ev.Data["to"].(string)
			on, _ := ev.Data["on"].(string)
			guard, _ := ev.Data["guard"].(string)
			implicit, _ := ev.Data["implicit"].(bool)
			items = append(items, timelineItem{
				Kind: "transition", From: from, To: to, On: on, Guard: guard, Implicit: implicit,
			})
		case journal.RunParked:
			prompt, _ := ev.Data["prompt"].(string)
			items = append(items, timelineItem{Kind: "park", State: state, Prompt: prompt})
		case journal.RunResumed:
			event, _ := ev.Data["event"].(string)
			items = append(items, timelineItem{Kind: "resume", Event: event})
		}
	}
	return items
}

// writtenFiles pulls path+size pairs out of a state's output: a single
// {path, bytes} (file.write / write-sugar) or a forEach aggregate whose items
// each carry one. The bytes themselves are never journaled.
func writtenFiles(output map[string]any) []writtenFile {
	var out []writtenFile
	add := func(v map[string]any) {
		if p, ok := v["path"].(string); ok && p != "" {
			n, _ := v["bytes"].(float64)
			out = append(out, writtenFile{Path: p, Bytes: int(n)})
		}
	}
	add(output)
	if items, ok := output["items"].([]any); ok {
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				add(m)
			}
		}
	}
	return out
}

// artifactRows surfaces files the run wrote, journal-only: path + byte count
// from a state's {path, bytes} output, plus the content when a journaled
// agent file.write tool call carried it (args.content). Write-sugar/action
// states journal no content, so those show path + size alone.
func artifactRows(rs *journal.RunState) []artifactRow {
	byPath := map[string]*artifactRow{}
	order := []string{}
	get := func(path, state string) *artifactRow {
		if a, ok := byPath[path]; ok {
			return a
		}
		a := &artifactRow{Path: path, State: state}
		byPath[path] = a
		order = append(order, path)
		return a
	}
	// Agent file.write tool calls carry the content in their args.
	for state, msgs := range rs.Convos {
		for _, m := range msgs {
			for _, tc := range m.ToolCalls {
				if tc.Name != "file.write" {
					continue
				}
				p, _ := tc.Args["path"].(string)
				if p == "" {
					continue
				}
				a := get(p, state)
				if content, ok := tc.Args["content"].(string); ok {
					a.Content, a.HasContent = content, true
					a.Bytes = len(content)
				}
			}
		}
	}
	// State outputs record {path, bytes} (no content).
	for state, v := range rs.Ctx {
		out, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for _, wf := range writtenFiles(out) {
			a := get(wf.Path, state)
			if a.Bytes == 0 {
				a.Bytes = wf.Bytes
			}
		}
	}
	sort.Strings(order)
	rows := make([]artifactRow, 0, len(order))
	for _, p := range order {
		rows = append(rows, *byPath[p])
	}
	return rows
}

// ---- resume -----------------------------------------------------------------

func (s *server) handleResume(c *echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	if _, busy := s.resuming.LoadOrStore(id, struct{}{}); busy {
		return echo.NewHTTPError(http.StatusConflict, "a resume is already in flight for this run")
	}
	release := func() { s.resuming.Delete(id) }

	run, err := s.store.GetRun(ctx, id)
	if err != nil {
		release()
		return echo.NewHTTPError(http.StatusNotFound, "run not found")
	}
	rs := s.fold(ctx, id)
	if rs == nil || rs.Parked == nil {
		release()
		return echo.NewHTTPError(http.StatusConflict, "run is not parked at a gate")
	}

	event, data, herr := gateFormAnswer(c, rs.Parked)
	if herr != nil {
		release()
		return herr
	}

	m, err := machine.ParseWithAssets(run.Source, run.Assets, parseOpts()...)
	if err != nil {
		release()
		return echo.NewHTTPError(http.StatusInternalServerError, "re-evaluating pinned machine: "+err.Error())
	}

	// A resume drives a real engine loop — agent states can run for minutes.
	// Never block the request: launch it, redirect, let meta-refresh show
	// progress. Errors land in the journal (handler_failed) and the run row.
	go func() {
		defer release()
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		_, err := s.eng.Resume(bg, m, id, event, data)
		if err != nil {
			s.eng.Listener.Warn("web resume failed", "run", id, "err", err.Error())
		}
	}()

	err = c.Redirect(http.StatusSeeOther, "/runs/"+id)
	if err != nil {
		return fmt.Errorf("redirecting to run %s: %w", id, err)
	}
	return nil
}

// gateFormAnswer maps the posted form to a resume (event, data), mirroring
// the CLI: single -> the chosen event; multi -> the gate's fixed event with
// the checked values; free-form -> a typed event. A note always merges in.
func gateFormAnswer(c *echo.Context, p *journal.ParkInfo) (string, map[string]any, *echo.HTTPError) {
	data := map[string]any{}
	if note := strings.TrimSpace(c.FormValue("note")); note != "" {
		data["note"] = note
	}

	ch := p.Choices
	if ch != nil && ch.Kind == "multi" {
		form, err := c.FormValues()
		if err != nil {
			return "", nil, echo.NewHTTPError(http.StatusBadRequest, "malformed form")
		}
		valid := map[string]bool{}
		for _, o := range ch.Options {
			valid[o.Value] = true
		}
		var selected []any
		for _, v := range form["selected"] {
			if valid[v] {
				selected = append(selected, v)
			}
		}
		if ch.Min > 0 && len(selected) < ch.Min {
			return "", nil, echo.NewHTTPError(http.StatusBadRequest, "select at least "+itoa(ch.Min)+" option(s)")
		}
		if ch.Max > 0 && len(selected) > ch.Max {
			return "", nil, echo.NewHTTPError(http.StatusBadRequest, "select at most "+itoa(ch.Max)+" option(s)")
		}
		data["selected"] = selected
		return ch.Event, data, nil
	}

	event := strings.TrimSpace(c.FormValue("event"))
	if event == "" {
		return "", nil, echo.NewHTTPError(http.StatusBadRequest, "choose an option")
	}
	return event, data, nil
}

// ---- helpers ----------------------------------------------------------------

// fold loads and folds a run's journal, swallowing errors (a run row without
// readable events is skipped, not fatal to the whole list).
func (s *server) fold(ctx context.Context, id string) *journal.RunState {
	events, err := s.store.Events(ctx, id)
	if err != nil {
		return nil
	}
	return journal.Fold(events)
}

// render executes a page template into the response.
func (s *server) render(c *echo.Context, code int, name string, data any) error {
	c.Response().Header().Set("Content-Type", "text/html; charset=utf-8")
	c.Response().WriteHeader(code)
	err := webTemplates.ExecuteTemplate(c.Response(), name, data)
	if err != nil {
		return fmt.Errorf("rendering template %s: %w", name, err)
	}
	return nil
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
