package web

// The jobs page's graph: a pipeline drawn the way Concourse draws one, laid
// out server-side. The model is a port of Concourse's createGraph
// (web/public/index.mjs): a node per job; an output node per (job, resource)
// it puts; an input node per (job, resource) it gets without passed:, merged
// when two land in one column; and a passed: constraint drawn from the
// upstream job's output node for that resource, or through a node of its own
// when the upstream only gets it. A put in one job and a get in another are
// NOT joined without passed:, because nothing makes the second wait for the
// first — an edge here is a promise of ordering.
//
// Layout lives here rather than in the browser because the page has no build
// system and no layout library — and none is needed: pipelines are small, so
// longest-path ranking, Concourse's pull-right and spacer passes, and a few
// barycenter sweeps are the whole algorithm.

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// Geometry, in px. The SVG is drawn at 1:1 and scrolls horizontally when a
// pipeline outgrows the pane, the same answer every wide table here uses.
const (
	dagCharW       = 7.3 // advance width of the 12px mono the nodes are set in
	dagPadX        = 12
	dagNodeH       = 44 // a job: name line plus status line
	dagResH        = 26 // a resource: its name
	dagPortPitch   = 18 // vertical room per edge landing on one side of a job
	dagHGap        = 48
	dagVGap        = 14
	dagMargin      = 8
	dagNameY       = 18 // first text baseline, relative to the node's top
	dagStatusY     = 34 // second text baseline
	dagResTextY    = 17
	dagMinWidth    = 80
	dagResMinWidth = 40
	dagRingGap     = 3
)

// graphNode is one placed node: a job, a resource, or a spacer — a ghost of
// a resource standing in a column an edge passes through.
type graphNode struct {
	ID   string
	Kind string // "job", "resource" or "spacer": what the template draws
	Name string // the job's name, or the resource's
	// The rest of the status fields are a job's alone.
	Status string // the shared status word, or "never ran"
	Glyph  string
	// StatusClass picks the node's color from the same st-* palette every
	// badge uses; "st-none" for a job that never ran.
	StatusClass string
	Running     bool
	// Line is the status line under the name: glyph, word, age, held.
	Line string
	// Text is what a resource or spacer box prints; a job prints Name and Line.
	Text string
	// CheckError is the first line of why a resource's last check failed.
	CheckError string
	// Label is the node's accessible name: in words, what its lines and its
	// edges say, since the drawing is the board's only form.
	Label string
	// Title is what hovering the node adds: how long the run took, since
	// when the breaker holds the job, what a queued run waits on.
	Title string
	// Queued is a run waiting behind the latest one; a job that never ran says so in Status instead.
	Queued     *queuedJob
	Layer      int
	X, Y, W, H int
	// Ring is the running ring's box, a gap outside the node's own.
	RingX, RingY, RingW, RingH int
	// Text anchors, precomputed so the template stays arithmetic-free.
	TextX, NameY, StatusY int
}

// graphEdge is one drawn edge between two node IDs.
type graphEdge struct {
	From, To string
	Resource string
	// Manual marks an input that does not trigger its job: drawn dashed, the
	// second channel beside color that Concourse's trigger-false class is.
	Manual bool
	Title  string
	Path   string
}

// graphView is the whole drawing.
type graphView struct {
	W, H  int
	Nodes []graphNode
	Edges []graphEdge
}

type nodeKind int

const (
	kindJob nodeKind = iota
	kindInput
	kindOutput
	kindVia // a passed: constraint's resource, when the upstream job only gets it
	kindSpacer
)

type layoutNode struct {
	id    string
	kind  nodeKind
	name  string
	view  int    // index into the job views, for a job
	equiv string // unconstrained inputs of one resource merge per column
	dead  bool

	rank       int
	slot       int
	ins, outs  []int
	left, rght []string // port keys, top to bottom

	x, y, w, h int
}

type layoutEdge struct {
	from, to int
	key      string
	manual   bool
	title    string
	dead     bool
}

type layout struct {
	nodes []*layoutNode
	edges []*layoutEdge
	byID  map[string]int
	// failing is each resource whose last check failed, by name.
	failing map[string]store.CheckError
}

func (g *layout) add(id string, kind nodeKind, name string, view int) int {
	if at, ok := g.byID[id]; ok {
		return at
	}

	g.byID[id] = len(g.nodes)
	g.nodes = append(g.nodes, &layoutNode{id: id, kind: kind, name: name, view: view})

	return len(g.nodes) - 1
}

// connect adds an edge unless one already joins the two nodes, which is
// Concourse's addEdge rule: one drawn line per pair.
func (g *layout) connect(from, to int, key string, manual bool, title string) {
	for _, at := range g.nodes[to].ins {
		if g.edges[at].from == from && !g.edges[at].dead {
			return
		}
	}

	g.edges = append(g.edges, &layoutEdge{from: from, to: to, key: key, manual: manual, title: title})
	g.nodes[from].outs = append(g.nodes[from].outs, len(g.edges)-1)
	g.nodes[to].ins = append(g.nodes[to].ins, len(g.edges)-1)
}

func (g *layout) drop(at int) {
	edge := g.edges[at]
	edge.dead = true
	g.nodes[edge.from].outs = without(g.nodes[edge.from].outs, at)
	g.nodes[edge.to].ins = without(g.nodes[edge.to].ins, at)
}

func without(list []int, value int) []int {
	kept := list[:0:0]

	for _, v := range list {
		if v != value {
			kept = append(kept, v)
		}
	}

	return kept
}

// buildGraph lays out the board's job views as Concourse's pipeline graph,
// marking every resource in failing.
func buildGraph(views []jobView, failing map[string]store.CheckError) graphView {
	g := &layout{byID: map[string]int{}, failing: failing}

	g.model(views)
	g.rank()
	g.collapseInputs()
	g.addSpacers()

	columns := g.order()
	g.size(views)
	g.place(columns)

	return g.view(views, columns)
}

// model is createGraph: jobs, then their outputs, then passed: edges (which
// prefer an upstream's output node), then unconstrained inputs.
func (g *layout) model(views []jobView) {
	jobs := map[string]int{}
	for i, view := range views {
		jobs[view.Name] = g.add("job:"+view.Name, kindJob, view.Name, i)
	}

	for _, view := range views {
		for _, resource := range view.Outputs {
			out := g.add("out:"+view.Name+":"+resource, kindOutput, resource, -1)
			g.connect(jobs[view.Name], out, resource, false, view.Name+" puts "+resource)
		}
	}

	for _, view := range views {
		for _, input := range view.Inputs {
			g.modelPassed(jobs, view.Name, input)
		}
	}

	for _, view := range views {
		for _, input := range view.Inputs {
			if len(input.Passed) > 0 {
				continue
			}

			in := g.add("in:"+view.Name+":"+input.Resource, kindInput, input.Resource, -1)
			g.nodes[in].equiv = input.Resource
			g.connect(in, jobs[view.Name], input.Resource, !input.Trigger, inputTitle(input.Resource, view.Name, input.Trigger))
		}
	}
}

// modelPassed draws one input's passed: constraints, each from the upstream
// job's output node for the resource, or through a node of its own when the
// upstream only gets it.
func (g *layout) modelPassed(jobs map[string]int, job string, input config.JobInput) {
	for _, upstream := range input.Passed {
		up, ok := jobs[upstream]
		if !ok {
			continue
		}

		source, ok := g.byID["out:"+upstream+":"+input.Resource]
		if !ok {
			source = g.add("via:"+upstream+":"+input.Resource, kindVia, input.Resource, -1)
			g.connect(up, source, input.Resource, false, input.Resource+" from "+upstream)
		}

		g.connect(source, jobs[job], input.Resource, !input.Trigger,
			inputTitle(input.Resource, job, input.Trigger)+", once it has passed "+upstream)
	}
}

func inputTitle(resource, job string, trigger bool) string {
	if trigger {
		return resource + " triggers " + job
	}

	return resource + " is read by " + job + " and never triggers it"
}

// rank puts every node one column past its deepest source, then pulls each
// node right to sit just before the nearest thing it feeds — Concourse's
// computeRanks, which keeps an input beside its job instead of in column 0.
// A cycle, which validation should never let through, is broken rather than
// followed, so a bad pipeline renders instead of hanging.
func (g *layout) rank() {
	state := make([]int, len(g.nodes)) // 0 unvisited, 1 in progress, 2 done

	var visit func(i int) int

	visit = func(i int) int {
		switch state[i] {
		case 2:
			return g.nodes[i].rank
		case 1:
			return -1
		}

		state[i] = 1
		depth := 0

		for _, at := range g.nodes[i].ins {
			if d := visit(g.edges[at].from) + 1; d > depth {
				depth = d
			}
		}

		g.nodes[i].rank, state[i] = depth, 2

		return depth
	}

	for i := range g.nodes {
		visit(i)
	}

	g.pullRight()
}

// pullRight moves every node to the column just before the nearest thing it
// feeds, until nothing moves.
func (g *layout) pullRight() {
	for range g.nodes {
		moved := false

		for _, node := range g.nodes {
			if len(node.outs) == 0 {
				continue
			}

			latest := -1
			for _, at := range node.outs {
				if r := g.nodes[g.edges[at].to].rank - 1; latest == -1 || r < latest {
					latest = r
				}
			}

			if latest > node.rank {
				node.rank, moved = latest, true
			}
		}

		if !moved {
			return
		}
	}
}

// collapseInputs merges unconstrained inputs of one resource that landed in
// one column — Concourse's collapseEquivalentNodes. Two jobs side by side
// reading the same repo share its node; two in different columns each get
// their own, so neither edge has to cross the graph.
func (g *layout) collapseInputs() {
	chosen := map[string]int{}

	for i, node := range g.nodes {
		if node.equiv == "" {
			continue
		}

		key := fmt.Sprintf("%d\x00%s", node.rank, node.equiv)

		keep, ok := chosen[key]
		if !ok {
			chosen[key] = i

			continue
		}

		for _, at := range append([]int(nil), node.outs...) {
			edge := g.edges[at]
			g.drop(at)
			g.connect(keep, edge.to, edge.key, edge.manual, edge.title)
		}

		node.dead = true
	}
}

// addSpacers replaces an edge spanning more than one column with a chain
// through ghost copies of its resource — Concourse's addSpacingNodes — so no
// line is drawn through a column it does not belong to.
func (g *layout) addSpacers() {
	for at := range g.edges {
		edge := g.edges[at]
		if edge.dead {
			continue
		}

		from, to := g.nodes[edge.from], g.nodes[edge.to]

		span := to.rank - from.rank
		if span <= 1 {
			continue
		}

		g.drop(at)

		prev := edge.from
		for step := 1; step < span; step++ {
			gap := g.add(fmt.Sprintf("gap:%d:%d", at, step), kindSpacer, edge.key, -1)
			g.nodes[gap].rank = from.rank + step
			g.connect(prev, gap, edge.key, edge.manual, edge.title)
			prev = gap
		}

		g.connect(prev, edge.to, edge.key, edge.manual, edge.title)
	}
}

// order groups live nodes into columns and runs barycenter sweeps — down,
// up, down — so each node sits near what it connects to. Creation order (and
// so config order) breaks ties, which keeps the drawing stable across polls.
func (g *layout) order() [][]int {
	deepest := 0

	for _, node := range g.nodes {
		if !node.dead && node.rank > deepest {
			deepest = node.rank
		}
	}

	columns := make([][]int, deepest+1)

	for i, node := range g.nodes {
		if !node.dead {
			node.slot = len(columns[node.rank])
			columns[node.rank] = append(columns[node.rank], i)
		}
	}

	for range 2 {
		for rank := 1; rank <= deepest; rank++ {
			g.sweep(columns, rank, true)
		}

		for rank := deepest - 1; rank >= 0; rank-- {
			g.sweep(columns, rank, false)
		}
	}

	for rank := 1; rank <= deepest; rank++ {
		g.sweep(columns, rank, true)
	}

	return columns
}

// sweep orders one column by the mean slot of each node's neighbours in
// other columns — its sources when upstream, its targets otherwise.
func (g *layout) sweep(columns [][]int, rank int, upstream bool) {
	column := columns[rank]

	weights := map[int]float64{}
	for _, i := range column {
		weights[i] = g.barycenter(i, upstream)
	}

	sort.SliceStable(column, func(a, b int) bool { return weights[column[a]] < weights[column[b]] })

	for slot, i := range column {
		g.nodes[i].slot = slot
	}
}

func (g *layout) barycenter(i int, upstream bool) float64 {
	node := g.nodes[i]

	edges, sum, n := node.outs, 0.0, 0
	if upstream {
		edges = node.ins
	}

	for _, at := range edges {
		other := g.nodes[g.edges[at].to]
		if upstream {
			other = g.nodes[g.edges[at].from]
		}

		if other.rank == node.rank {
			continue
		}

		sum += float64(other.slot)
		n++
	}

	if n == 0 {
		return float64(node.slot)
	}

	return sum / float64(n)
}

// size gives every node its box. A job's ports — one per resource landing on
// each side, ordered by where the other end sits — set its height, so edges
// meet it at separate points the way Concourse's do.
func (g *layout) size(views []jobView) {
	for _, node := range g.nodes {
		if node.dead {
			continue
		}

		if node.kind != kindJob {
			node.w = max(int(dagCharW*float64(utf8.RuneCountInString(g.resourceText(node))))+2*dagPadX, dagResMinWidth)
			node.h = dagResH

			continue
		}

		node.left = g.ports(node.ins, true)
		node.rght = g.ports(node.outs, false)
		node.w = nodeWidth(views[node.view])
		node.h = max(dagNodeH, max(len(node.left), len(node.rght))*dagPortPitch+8)
	}
}

// resourceText is what a resource box says: its name, after a ! when its
// last check failed. A spacer stands for an edge, not the resource, so it
// carries no mark.
func (g *layout) resourceText(node *layoutNode) string {
	if node.kind == kindSpacer {
		return node.name
	}

	if _, ok := g.failing[node.name]; ok {
		return "! " + node.name
	}

	return node.name
}

func (g *layout) ports(edges []int, upstream bool) []string {
	type port struct {
		key  string
		slot int
	}

	var ports []port

	seen := map[string]bool{}

	for _, at := range edges {
		edge := g.edges[at]
		if seen[edge.key] {
			continue
		}

		seen[edge.key] = true

		other := g.nodes[edge.to]
		if upstream {
			other = g.nodes[edge.from]
		}

		ports = append(ports, port{edge.key, other.slot})
	}

	sort.SliceStable(ports, func(a, b int) bool { return ports[a].slot < ports[b].slot })

	keys := make([]string, len(ports))
	for i, p := range ports {
		keys[i] = p.key
	}

	return keys
}

// portY is where an edge of key meets node on one side: evenly spaced ports
// on a job, the middle of anything else.
func (n *layoutNode) portY(key string, left bool) int {
	keys := n.rght
	if left {
		keys = n.left
	}

	for i, k := range keys {
		if k == key {
			return n.y + n.h*(2*i+1)/(2*len(keys))
		}
	}

	return n.y + n.h/2
}

// place sets coordinates: columns left to right, each as wide as its widest
// node, then each node pulled level with what it connects to without
// overlapping its neighbours, so a chain of one input, one job and one output
// reads as a straight line.
func (g *layout) place(columns [][]int) {
	x := dagMargin

	for _, column := range columns {
		if len(column) == 0 {
			continue
		}

		width := 0
		for _, i := range column {
			width = max(width, g.nodes[i].w)
		}

		y := dagMargin

		for _, i := range column {
			node := g.nodes[i]
			node.x = x + (width-node.w)/2
			node.y = y
			y += node.h + dagVGap
		}

		x += width + dagHGap
	}

	for rank := 1; rank < len(columns); rank++ {
		g.align(columns[rank], true, false)
	}

	for rank := len(columns) - 2; rank >= 0; rank-- {
		g.align(columns[rank], false, true)
	}

	for rank := 1; rank < len(columns); rank++ {
		g.align(columns[rank], true, false)
	}

	g.lift()
}

// lift moves the drawing up to its top margin: aligning only ever pushes a
// node down, so every node can end up below an empty band.
func (g *layout) lift() {
	top := -1

	for _, node := range g.nodes {
		if !node.dead && (top == -1 || node.y < top) {
			top = node.y
		}
	}

	for _, node := range g.nodes {
		node.y -= top - dagMargin
	}
}

// align moves each node in a column level with the ports it connects to on
// one side, top to bottom, never closer than dagVGap to the node above. With
// sourcesOnly it moves only nodes nothing feeds, so a right-to-left pass
// lines inputs up with their jobs without undoing the left-to-right one.
func (g *layout) align(column []int, upstream, sourcesOnly bool) {
	next := dagMargin

	for _, i := range column {
		node := g.nodes[i]
		want := node.y

		if !sourcesOnly || len(node.ins) == 0 {
			want = g.levelWith(node, upstream)
		}

		node.y = max(want, next)
		next = node.y + node.h + dagVGap
	}
}

// levelWith is the top a node would need for its ports on one side to meet
// the ports at the other end of its edges, averaged; its own top when it has
// no edges on that side.
func (g *layout) levelWith(node *layoutNode, upstream bool) int {
	edges := node.outs
	if upstream {
		edges = node.ins
	}

	if len(edges) == 0 {
		return node.y
	}

	sum := 0

	for _, at := range edges {
		edge := g.edges[at]

		if upstream {
			sum += g.nodes[edge.from].portY(edge.key, false) - (node.portY(edge.key, true) - node.y)
		} else {
			sum += g.nodes[edge.to].portY(edge.key, true) - (node.portY(edge.key, false) - node.y)
		}
	}

	return sum / len(edges)
}

// view hands the template its nodes column by column, top to bottom — the
// order a reader scans and a keyboard tabs, and already each column's order,
// since align never moves a node above the one before it — and its edges as
// paths.
func (g *layout) view(views []jobView, columns [][]int) graphView {
	var out graphView

	for _, column := range columns {
		for _, i := range column {
			node := g.nodes[i]
			out.Nodes = append(out.Nodes, g.placed(node, views))
			out.W = max(out.W, node.x+node.w+dagMargin)
			out.H = max(out.H, node.y+node.h+dagMargin)
		}
	}

	for _, edge := range g.edges {
		if edge.dead {
			continue
		}

		from, to := g.nodes[edge.from], g.nodes[edge.to]
		x1, y1 := from.x+from.w, from.portY(edge.key, false)
		x2, y2 := to.x, to.portY(edge.key, true)
		mid := (x1 + x2) / 2

		out.Edges = append(out.Edges, graphEdge{
			From:     from.id,
			To:       to.id,
			Resource: edge.key,
			Manual:   edge.manual,
			Title:    edge.title,
			Path:     fmt.Sprintf("M%d,%d C%d,%d %d,%d %d,%d", x1, y1, mid, y1, mid, y2, x2, y2),
		})
	}

	return out
}

func (g *layout) placed(node *layoutNode, views []jobView) graphNode {
	placed := graphNode{
		ID:    node.id,
		Name:  node.name,
		Layer: node.rank,
		X:     node.x,
		Y:     node.y,
		W:     node.w,
		H:     node.h,
		TextX: node.x + dagPadX,
	}

	switch node.kind {
	case kindJob:
		view := views[node.view]
		word, glyph, _ := nodeStatus(view)

		placed.Kind = "job"
		placed.Status, placed.Glyph, placed.StatusClass = word, glyph, finishedClass(view)
		placed.Running = view.HasRun && statusWord(view.Latest.Status) == "running"
		placed.Line = statusLine(view, time.Now())
		placed.Title = nodeTitle(view)
		placed.Label = jobLabel(view, time.Now())
		placed.Queued = queuedBehind(view)
		placed.RingX, placed.RingY = node.x-dagRingGap, node.y-dagRingGap
		placed.RingW, placed.RingH = node.w+2*dagRingGap, node.h+2*dagRingGap
		placed.NameY = node.y + dagNameY
		placed.StatusY = node.y + dagStatusY
	case kindSpacer:
		placed.Kind = "spacer"
		placed.Text = node.name
		placed.NameY = node.y + dagResTextY
	case kindInput, kindOutput, kindVia:
		placed.Kind = "resource"
		placed.Label = "resource " + node.name
		placed.Text = g.resourceText(node)

		if failed, ok := g.failing[node.name]; ok {
			placed.CheckError, _, _ = strings.Cut(failed.Message, "\n")
			placed.Label += ", check failing"
		}
		placed.NameY = node.y + dagResTextY
	}

	return placed
}

func nodeWidth(view jobView) int {
	chars := max(utf8.RuneCountInString(view.Name), utf8.RuneCountInString(statusLine(view, time.Time{}))+dagAgeChars)
	if queuedBehind(view) != nil {
		chars += utf8.RuneCountInString(queuedSuffix)
	}

	return max(int(dagCharW*float64(chars))+2*dagPadX, dagMinWidth)
}

// dagAgeChars is the room a node keeps for its age ("59m", "23h"), so a
// column does not widen the poll the age gains a digit.
const dagAgeChars = 4

// statusLine is a job node's second line: what its latest run is doing or
// did, how long ago it started, and the breaker when it holds the job. now
// zero leaves the age out, which is how nodeWidth measures the rest.
func statusLine(view jobView, now time.Time) string {
	word, glyph, _ := nodeStatus(view)

	line := glyph + " " + word

	if view.HasRun && !now.IsZero() && !view.Latest.StartedAt.IsZero() {
		age, _ := agoCompact(now.Sub(view.Latest.StartedAt))
		line += " " + age
	}

	if view.Held {
		line += " · held"
	}

	return line
}

// nodeTitle is the detail a node's two lines leave out — what the list view's
// duration and held columns said before the graph was the only view.
func nodeTitle(view jobView) string {
	var parts []string

	if view.HasRun {
		verb := "took"
		if !finished(view.Latest.Status) {
			verb = "running for"
		}

		parts = append(parts, "latest run "+verb+" "+formatDuration(view.Latest.Duration()))
	}

	if view.Held {
		plural := "s"
		if view.Failures == 1 {
			plural = ""
		}

		held := fmt.Sprintf("held after %d failure%s", view.Failures, plural)
		at, err := time.Parse(time.RFC3339Nano, view.HeldAt)
		if err == nil {
			held += ", " + formatAgo(at)
		}

		parts = append(parts, held)
	}

	if view.Queued != nil {
		waiting := "starting"
		if view.Queued.WaitingOn != "" {
			waiting = "waiting: " + view.Queued.WaitingOn
		}

		parts = append(parts, "a run is queued, "+waiting)
	}

	return strings.Join(parts, "; ")
}

// jobLabel is a job node's accessible name: "draft, passed 15 hours ago,
// triggered by weekday, writes draft".
func jobLabel(view jobView, now time.Time) string {
	return strings.Join(append(append([]string{view.Name}, spokenStatus(view, now)...), spokenEdges(view)...), ", ")
}

// spokenStatus is what a node's status line and ring say, in words.
func spokenStatus(view jobView, now time.Time) []string {
	word, _, _ := nodeStatus(view)
	parts := []string{word}

	if view.HasRun && !view.Latest.StartedAt.IsZero() {
		_, spoken := agoCompact(now.Sub(view.Latest.StartedAt))
		if finished(view.Latest.Status) {
			parts[0] += " " + spoken
		} else {
			parts = append(parts, "started "+spoken)
		}
	}

	if run, ok := lastFinished(view); ok && !finished(view.Latest.Status) {
		parts = append(parts, "last finished "+statusWord(run.Status))
	}

	if view.Held {
		parts = append(parts, "held")
	}

	if queued := queuedBehind(view); queued != nil {
		waiting := "starting"
		if queued.WaitingOn != "" {
			waiting = "waiting: " + queued.WaitingOn
		}

		parts = append(parts, "queued", waiting)
	}

	return parts
}

// spokenEdges is what a node's edges say, in words.
func spokenEdges(view jobView) []string {
	var parts, triggers, reads []string

	for _, input := range view.Inputs {
		name := input.Resource
		if len(input.Passed) > 0 {
			name += " once it has passed " + spokenList(input.Passed)
		}

		if input.Trigger {
			triggers = append(triggers, name)
		} else {
			reads = append(reads, name)
		}
	}

	if len(triggers) > 0 {
		parts = append(parts, "triggered by "+spokenList(triggers))
	}

	if len(reads) > 0 {
		parts = append(parts, "reads "+spokenList(reads))
	}

	if len(view.Outputs) > 0 {
		parts = append(parts, "writes "+spokenList(view.Outputs))
	}

	return parts
}

// spokenList joins names the way a sentence does: "a", "a and b", "a, b and c".
func spokenList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}

	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// finishedClass colors a node by its latest FINISHED run — Concourse's
// dashboard rule, the one pipelineMark folds by — so a red job being rebuilt
// stays red under its running ring.
func finishedClass(view jobView) string {
	if run, ok := lastFinished(view); ok {
		return "st-" + statusWord(run.Status)
	}

	if !view.HasRun && view.Queued != nil {
		return "st-queued"
	}

	return "st-none"
}

func lastFinished(view jobView) (store.RunRow, bool) {
	if view.HasFinished {
		return view.Finished, true
	}

	if view.HasRun && finished(view.Latest.Status) {
		return view.Latest, true
	}

	return store.RunRow{}, false
}

// nodeStatus is the graph's reading of a job's latest run: the shared status
// word, its glyph, and the st-* class that colors the node.
func nodeStatus(view jobView) (word, glyph, class string) {
	if !view.HasRun && view.Queued != nil {
		return "queued", "○", "st-queued"
	}

	if !view.HasRun {
		return "never ran", "○", "st-none"
	}

	word = statusWord(view.Latest.Status)

	// One glyph vocabulary: the browser tab's statusMark, plus the graph's
	// own fallback for the words the tab never shows (pending).
	if glyph = statusMark(word); glyph == "" {
		glyph = "○"
	}

	return word, glyph, "st-" + word
}

// queuedSuffix is what a queued run adds to a node's status line, counted for its width.
const queuedSuffix = " · ○ queued"

// queuedBehind is the run queued behind a job's latest one, nil when there is none or when it is the only thing the node has to say.
func queuedBehind(view jobView) *queuedJob {
	if !view.HasRun {
		return nil
	}

	return view.Queued
}
