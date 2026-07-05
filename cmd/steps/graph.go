package main

// The state-machine diagram: a layered, top-to-bottom SVG of the machine's
// topology (machine.GraphView) with a specific run's progress overlaid. It is
// generated server-side on every handleRun so it survives the page's
// meta-refresh; all color comes from CSS classes (see layout.html) so light/
// dark and the run-status palette apply for free. Layout is deliberately
// pragmatic — BFS ranks + one barycenter sweep — which is plenty for the
// handful-of-states machines steps runs; deeper crossing-minimization is out of
// scope.

import (
	"fmt"
	"html/template"
	"sort"
	"strings"

	"github.com/jtarchie/steps/machine"
)

// RunOverlay projects one run's progress onto the static topology. A zero
// value renders a plain structural diagram (no run).
type RunOverlay struct {
	Visits  map[string]int     // state -> entry count (absent/0 = unvisited)
	Failed  map[string]bool    // states that recorded a handler_failed
	Fired   map[[2]string]bool // {from,to} transitions that actually fired
	Current string             // the run's current state
	Parked  bool               // the current state is a parked human gate
	Status  string             // run status (unused by layout; carried for callers)
}

func (o RunOverlay) started() bool { return len(o.Visits) > 0 }

const (
	gvNodeH    = 34.0
	gvCharW    = 7.0
	gvNodePadX = 18.0
	gvMinNodeW = 72.0
	gvRankGap  = 60.0
	gvNodeGap  = 30.0
	gvMargin   = 24.0
	gvBow      = 46.0 // how far loop/back edges bow out to the side
)

type nodeBox struct {
	machine.GraphNode
	rank                             int
	order                            int
	x, y, w, h                       float64
	visited, current, failed, parked bool
	visits                           int
}

type edgePath struct {
	machine.GraphEdge
	back   bool
	fired  bool
	d      string // SVG path data
	lx, ly float64
}

type gvLayout struct {
	nodes  []*nodeBox
	edges  []edgePath
	byName map[string]*nodeBox
	disco  map[string]int // BFS discovery order, for stable intra-rank seeding
	w, h   float64
}

// buildLayout ranks nodes by BFS distance from Initial, orders each rank with a
// single barycenter sweep, assigns coordinates, and routes edges.
func buildLayout(g machine.GraphView, ov RunOverlay) *gvLayout {
	lay := &gvLayout{byName: make(map[string]*nodeBox, len(g.Nodes))}
	for i := range g.Nodes {
		n := g.Nodes[i]
		nb := &nodeBox{GraphNode: n, rank: -1, w: nodeWidth(n.Name), h: gvNodeH}
		nb.visits = ov.Visits[n.Name]
		nb.visited = nb.visits > 0
		nb.failed = ov.Failed[n.Name]
		nb.current = n.Name == ov.Current
		nb.parked = ov.Parked && nb.current
		lay.nodes = append(lay.nodes, nb)
		lay.byName[n.Name] = nb
	}

	adj := map[string][]string{}
	for _, e := range g.Edges {
		if lay.byName[e.From] != nil && lay.byName[e.To] != nil {
			adj[e.From] = append(adj[e.From], e.To)
		}
	}

	maxRank := assignRanks(lay, g.Initial, adj)
	byRank := orderRanks(lay, g.Edges, maxRank)
	placeNodes(lay, byRank, maxRank)
	routeEdges(lay, g.Edges, ov)
	return lay
}

// assignRanks does a BFS from Initial (rank = shortest hop count), recording a
// discovery order for stable intra-rank seeding. Any node the BFS misses (it
// shouldn't — validation proves reachability) sinks below the deepest rank so
// it still renders. Returns the deepest rank.
func assignRanks(lay *gvLayout, initial string, adj map[string][]string) int {
	disco := map[string]int{}
	seq := 0
	start := initial
	if lay.byName[start] == nil && len(lay.nodes) > 0 {
		start = lay.nodes[0].Name
	}
	if nb := lay.byName[start]; nb != nil {
		nb.rank = 0
		disco[start] = seq
		seq++
		queue := []string{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			cr := lay.byName[cur].rank
			for _, to := range adj[cur] {
				tb := lay.byName[to]
				if tb.rank == -1 {
					tb.rank = cr + 1
					disco[to] = seq
					seq++
					queue = append(queue, to)
				}
			}
		}
	}
	maxRank := 0
	for _, nb := range lay.nodes {
		if nb.rank > maxRank {
			maxRank = nb.rank
		}
	}
	for _, nb := range lay.nodes {
		if nb.rank == -1 {
			maxRank++
			nb.rank = maxRank
			disco[nb.Name] = seq
			seq++
		}
	}
	lay.disco = disco
	return maxRank
}

// orderRanks orders the nodes within each rank to reduce edge crossings. It
// seeds by BFS discovery order (which preserves pipe order), then runs a few
// bidirectional barycenter sweeps — down by predecessor position, up by
// successor position — keeping the arrangement with the fewest crossings.
// Deterministic and never worse than the seed. Enough for the small graphs
// steps runs; not optimal crossing-minimization.
func orderRanks(lay *gvLayout, edges []machine.GraphEdge, maxRank int) map[int][]*nodeBox {
	byRank := groupByRank(lay)
	preds, succs, fwd := forwardAdjacency(lay, edges)

	for r := 0; r <= maxRank; r++ {
		row := byRank[r]
		sort.SliceStable(row, func(i, j int) bool { return lay.disco[row[i].Name] < lay.disco[row[j].Name] })
	}
	orderIdx := reindexAll(byRank, maxRank)
	best, bestX := snapshotOrder(byRank, maxRank), countCrossings(fwd, lay, orderIdx)

	for range 4 {
		for r := 1; r <= maxRank; r++ {
			barySort(byRank[r], preds, orderIdx)
			reindexRow(byRank[r], orderIdx)
		}
		for r := maxRank - 1; r >= 0; r-- {
			barySort(byRank[r], succs, orderIdx)
			reindexRow(byRank[r], orderIdx)
		}
		if x := countCrossings(fwd, lay, orderIdx); x < bestX {
			bestX, best = x, snapshotOrder(byRank, maxRank)
		}
	}
	applyOrder(byRank, best, maxRank)
	return byRank
}

func groupByRank(lay *gvLayout) map[int][]*nodeBox {
	byRank := map[int][]*nodeBox{}
	for _, nb := range lay.nodes {
		byRank[nb.rank] = append(byRank[nb.rank], nb)
	}
	return byRank
}

// forwardAdjacency returns predecessor and successor name lists over forward
// edges only (back edges don't constrain within-rank order), plus the forward
// edge slice used for crossing counting.
func forwardAdjacency(lay *gvLayout, edges []machine.GraphEdge) (preds, succs map[string][]string, fwd []machine.GraphEdge) {
	preds, succs = map[string][]string{}, map[string][]string{}
	for _, e := range edges {
		f, t := lay.byName[e.From], lay.byName[e.To]
		if f != nil && t != nil && t.rank > f.rank {
			preds[e.To] = append(preds[e.To], e.From)
			succs[e.From] = append(succs[e.From], e.To)
			fwd = append(fwd, e)
		}
	}
	return preds, succs, fwd
}

// barySort orders a rank by the mean index of each node's forward neighbors;
// a node with no neighbor in `neigh` keeps its current index.
func barySort(row []*nodeBox, neigh map[string][]string, orderIdx map[string]int) {
	bary := map[string]float64{}
	for _, nb := range row {
		ns := neigh[nb.Name]
		if len(ns) == 0 {
			bary[nb.Name] = float64(orderIdx[nb.Name])
			continue
		}
		sum := 0.0
		for _, n := range ns {
			sum += float64(orderIdx[n])
		}
		bary[nb.Name] = sum / float64(len(ns))
	}
	sort.SliceStable(row, func(i, j int) bool { return bary[row[i].Name] < bary[row[j].Name] })
}

func reindexRow(row []*nodeBox, orderIdx map[string]int) {
	for i, nb := range row {
		orderIdx[nb.Name] = i
		nb.order = i
	}
}

func reindexAll(byRank map[int][]*nodeBox, maxRank int) map[string]int {
	orderIdx := map[string]int{}
	for r := 0; r <= maxRank; r++ {
		reindexRow(byRank[r], orderIdx)
	}
	return orderIdx
}

func snapshotOrder(byRank map[int][]*nodeBox, maxRank int) map[string]int {
	out := map[string]int{}
	for r := 0; r <= maxRank; r++ {
		for i, nb := range byRank[r] {
			out[nb.Name] = i
		}
	}
	return out
}

func applyOrder(byRank map[int][]*nodeBox, order map[string]int, maxRank int) {
	for r := 0; r <= maxRank; r++ {
		row := byRank[r]
		sort.SliceStable(row, func(i, j int) bool { return order[row[i].Name] < order[row[j].Name] })
		for i, nb := range row {
			nb.order = i
		}
	}
}

// countCrossings counts pairs of forward edges that cross given the current
// within-rank ordering: two edges spanning the same rank pair cross when their
// endpoints sit in opposite left-right order. O(E²), trivial at these sizes.
func countCrossings(fwd []machine.GraphEdge, lay *gvLayout, orderIdx map[string]int) int {
	n := 0
	for i := range fwd {
		for j := i + 1; j < len(fwd); j++ {
			a, b := fwd[i], fwd[j]
			if lay.byName[a.From].rank != lay.byName[b.From].rank ||
				lay.byName[a.To].rank != lay.byName[b.To].rank {
				continue
			}
			df := orderIdx[a.From] - orderIdx[b.From]
			dt := orderIdx[a.To] - orderIdx[b.To]
			if (df < 0 && dt > 0) || (df > 0 && dt < 0) {
				n++
			}
		}
	}
	return n
}

// placeNodes centers each rank horizontally around the widest rank and stacks
// ranks top-to-bottom, then records the diagram extent.
func placeNodes(lay *gvLayout, byRank map[int][]*nodeBox, maxRank int) {
	rowWidth := func(row []*nodeBox) float64 {
		if len(row) == 0 {
			return 0
		}
		w := 0.0
		for _, nb := range row {
			w += nb.w
		}
		return w + gvNodeGap*float64(len(row)-1)
	}
	maxRow := 0.0
	for r := 0; r <= maxRank; r++ {
		if w := rowWidth(byRank[r]); w > maxRow {
			maxRow = w
		}
	}
	for r := 0; r <= maxRank; r++ {
		row := byRank[r]
		x := gvMargin + (maxRow-rowWidth(row))/2
		y := gvMargin + float64(r)*(gvNodeH+gvRankGap)
		for _, nb := range row {
			nb.x, nb.y = x, y
			x += nb.w + gvNodeGap
		}
	}
	lay.w = gvMargin*2 + maxRow
	lay.h = gvMargin*2 + float64(maxRank+1)*gvNodeH + float64(maxRank)*gvRankGap
}

// routeEdges builds a path per edge: forward edges curve down, back edges and
// self-loops bow out to the right. Parallel edges between the same pair (e.g. a
// gate's rejected and timeout both to failed) are fanned apart so their paths
// and labels don't stack. The diagram width grows to fit any bow.
func routeEdges(lay *gvLayout, edges []machine.GraphEdge, ov RunOverlay) {
	total := map[[2]string]int{}
	for _, e := range edges {
		if lay.byName[e.From] != nil && lay.byName[e.To] != nil {
			total[[2]string{e.From, e.To}]++
		}
	}
	seen := map[[2]string]int{}

	maxX := lay.w
	for _, e := range edges {
		f, t := lay.byName[e.From], lay.byName[e.To]
		if f == nil || t == nil {
			continue
		}
		key := [2]string{e.From, e.To}
		idx, n := seen[key], total[key]
		seen[key]++
		off := (float64(idx) - float64(n-1)/2) * 16 // fan parallel edges apart

		ep := edgePath{GraphEdge: e, fired: ov.Fired[key]}
		var reach float64
		ep.d, ep.lx, ep.ly, ep.back, reach = routePath(f, t, off)
		ep.ly += float64(idx) * 13 // stagger stacked labels
		if reach > maxX {
			maxX = reach
		}
		// Grow the viewBox so a left-anchored label never clips on the right.
		if e.Label != "" {
			if x := ep.lx + float64(len(e.Label))*gvCharW + 8; x > maxX {
				maxX = x
			}
		}
		lay.edges = append(lay.edges, ep)
	}
	if maxX > lay.w {
		lay.w = maxX + gvMargin
	}
}

// routePath picks the shape for an edge — self-loop, forward (down), or back
// (up/sideways) — and returns its path, label anchor, back flag, and the
// rightmost x it reaches (so the caller can grow the viewBox to fit any bow).
func routePath(f, t *nodeBox, off float64) (d string, lx, ly float64, back bool, reach float64) {
	switch {
	case f == t:
		d, lx, ly = selfLoopPath(f)
		return d, lx, ly, true, f.x + f.w + gvBow + 12
	case t.rank > f.rank:
		d, lx, ly = forwardPath(f, t, off)
		return d, lx, ly, false, 0
	default:
		d, lx, ly = backPath(f, t, off)
		edge := f.x + f.w
		if r := t.x + t.w; r > edge {
			edge = r
		}
		return d, lx, ly, true, edge + gvBow + off + 12
	}
}

func forwardPath(f, t *nodeBox, off float64) (string, float64, float64) {
	sx, sy := f.x+f.w/2+off, f.y+f.h
	tx, ty := t.x+t.w/2+off, t.y
	d := fmt.Sprintf("M%.1f,%.1f C%.1f,%.1f %.1f,%.1f %.1f,%.1f",
		sx, sy, sx, sy+gvRankGap*0.5, tx, ty-gvRankGap*0.5, tx, ty)
	// Labels are left-anchored a few px right of the edge so they grow into
	// open space instead of clipping off the left of the viewBox.
	return d, (sx+tx)/2 + 6, (sy+ty)/2 + 3
}

func backPath(f, t *nodeBox, off float64) (string, float64, float64) {
	sx, sy := f.x+f.w, f.y+f.h/2
	tx, ty := t.x+t.w, t.y+t.h/2
	bx := sx
	if tx > bx {
		bx = tx
	}
	bx += gvBow + off
	d := fmt.Sprintf("M%.1f,%.1f C%.1f,%.1f %.1f,%.1f %.1f,%.1f", sx, sy, bx, sy, bx, ty, tx, ty)
	return d, bx + 4, (sy + ty) / 2
}

func selfLoopPath(f *nodeBox) (string, float64, float64) {
	x := f.x + f.w
	bx := x + gvBow
	d := fmt.Sprintf("M%.1f,%.1f C%.1f,%.1f %.1f,%.1f %.1f,%.1f",
		x, f.y+f.h*0.3, bx, f.y-8, bx, f.y+f.h+8, x, f.y+f.h*0.7)
	return d, bx + 4, f.y + f.h/2
}

func nodeWidth(label string) float64 {
	w := float64(len(label))*gvCharW + gvNodePadX*2
	if w < gvMinNodeW {
		w = gvMinNodeW
	}
	return w
}

// renderGraphSVG builds the inline SVG. Node names are validated identifiers,
// but edge labels/guards come from arbitrary JS via Dyn.Display(), so every
// text/attribute value is HTML-escaped.
func renderGraphSVG(g machine.GraphView, ov RunOverlay) template.HTML {
	if len(g.Nodes) == 0 {
		return ""
	}
	lay := buildLayout(g, ov)

	var b strings.Builder
	root := "graph-svg"
	if ov.started() {
		root += " has-run"
	}
	fmt.Fprintf(&b, `<svg class="%s" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="state machine %s">`,
		root, lay.w, lay.h, lay.w, lay.h, esc(g.Name))
	b.WriteString(`<defs>` +
		`<marker id="gv-arrow" markerWidth="9" markerHeight="9" refX="7.5" refY="3" orient="auto"><path d="M0,0 L7.5,3 L0,6 Z" class="gv-arrowhead"/></marker>` +
		`<marker id="gv-arrow-fired" markerWidth="9" markerHeight="9" refX="7.5" refY="3" orient="auto"><path d="M0,0 L7.5,3 L0,6 Z" class="gv-arrowhead-fired"/></marker>` +
		`</defs>`)

	// Edges first so nodes paint over their endpoints.
	for _, e := range lay.edges {
		cls := "edge edge-" + string(e.Kind)
		marker := "gv-arrow"
		if e.fired {
			cls += " fired"
			marker = "gv-arrow-fired"
		}
		if e.back {
			cls += " back"
		}
		fmt.Fprintf(&b, `<g class="%s"><path d="%s" fill="none" marker-end="url(#%s)"/>`, cls, e.d, marker)
		if e.Label != "" {
			fmt.Fprintf(&b, `<text class="edge-label" x="%.1f" y="%.1f" text-anchor="start">%s</text>`, e.lx, e.ly, esc(e.Label))
		}
		b.WriteString(`</g>`)
	}

	for _, n := range lay.nodes {
		fmt.Fprintf(&b, `<g class="%s" data-name="%s" data-kind="%s" data-model="%s" data-visits="%d" data-status="%s">`,
			esc(nodeClasses(n)), esc(n.Name), esc(n.Kind), esc(n.Model), n.visits, esc(nodeStatusText(n)))
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="7"/>`, n.x, n.y, n.w, n.h)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" dominant-baseline="middle">%s</text>`,
			n.x+n.w/2, n.y+n.h/2, esc(n.Name))
		fmt.Fprintf(&b, `<title>%s</title>`, esc(nodeTooltip(n)))
		b.WriteString(`</g>`)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // every dynamic value is HTMLEscapeString'd above
}

func nodeClasses(n *nodeBox) string {
	cls := "node kind-" + n.Kind
	if n.Terminal && n.Status == "failed" {
		cls += " status-failed"
	}
	if n.Initial {
		cls += " initial"
	}
	if n.visited {
		cls += " visited"
	}
	if n.current {
		cls += " current"
	}
	if n.failed {
		cls += " failed"
	}
	if n.parked {
		cls += " parked"
	}
	return cls
}

// nodeStatusText is the short run-state label the JS hover panel shows.
func nodeStatusText(n *nodeBox) string {
	switch {
	case n.parked:
		return "parked"
	case n.current:
		return "current"
	case n.failed:
		return "failed"
	case n.visited:
		return fmt.Sprintf("visited ×%d", n.visits)
	default:
		return "idle"
	}
}

// nodeTooltip is the native SVG <title> shown on hover without JS.
func nodeTooltip(n *nodeBox) string {
	parts := []string{n.Name + " · " + n.Kind}
	if n.Model != "" {
		parts = append(parts, "model: "+n.Model)
	}
	if n.Terminal {
		s := "success"
		if n.Status == "failed" {
			s = "failed"
		}
		parts = append(parts, "terminal ("+s+")")
	}
	if n.visits > 0 {
		parts = append(parts, fmt.Sprintf("visited ×%d", n.visits))
	}
	if n.current {
		parts = append(parts, "← current")
	}
	if n.parked {
		parts = append(parts, "parked at a gate")
	}
	if n.failed {
		parts = append(parts, "recorded a failure")
	}
	return strings.Join(parts, "\n")
}

func esc(s string) string { return template.HTMLEscapeString(s) }
