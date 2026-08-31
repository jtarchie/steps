package web

// The jobs board's graph view: the passed: constraint DAG laid out as
// columns, server-side. Layout lives here rather than in the browser because
// the page has no build system and no layout library — and none is needed:
// pipelines are small (tens of jobs), so longest-path layering plus one
// barycenter pass is the whole algorithm.

import (
	"fmt"
	"sort"
)

// Geometry, in px. The SVG is drawn at 1:1 and scrolls horizontally when a
// pipeline outgrows the pane, the same answer every wide table here uses.
const (
	dagCharW    = 7.3 // advance width of the 12px mono the nodes are set in
	dagPadX     = 12
	dagNodeH    = 44
	dagHGap     = 56
	dagVGap     = 16
	dagMargin   = 8
	dagNameY    = 18 // first text baseline, relative to the node's top
	dagStatusY  = 34 // second text baseline
	dagMinWidth = 80
)

// graphNode is one job, placed.
type graphNode struct {
	Name   string
	Status string // the shared status word, or "never ran"
	Glyph  string
	// StatusClass picks the node's color from the same st-* palette every
	// badge uses; "st-none" for a job that never ran.
	StatusClass string
	Running     bool
	Layer       int
	X, Y, W, H  int
	// Text anchors, precomputed so the template stays arithmetic-free.
	TextX, NameY, StatusY int
}

// graphEdge is one passed: constraint, as a drawn path.
type graphEdge struct {
	From, To string
	Resource string
	Path     string
}

// graphView is the whole drawing.
type graphView struct {
	W, H  int
	Nodes []graphNode
	Edges []graphEdge
}

// buildGraph lays out the board's job views as a left-to-right DAG.
func buildGraph(views []jobView) graphView {
	byName := map[string]int{}
	for i, view := range views {
		byName[view.Name] = i
	}

	layers := layerByLongestPath(views, byName)

	// Group into columns, keeping config order as the initial vertical order.
	columns := map[int][]int{}
	deepest := 0

	for i := range views {
		layer := layers[i]
		columns[layer] = append(columns[layer], i)

		if layer > deepest {
			deepest = layer
		}
	}

	orderColumns(views, byName, layers, columns, deepest)

	nodes := placeNodes(views, columns, deepest)

	width, height := 0, 0
	for _, node := range nodes {
		if r := node.X + node.W + dagMargin; r > width {
			width = r
		}

		if b := node.Y + node.H + dagMargin; b > height {
			height = b
		}
	}

	return graphView{W: width, H: height, Nodes: nodes, Edges: graphEdges(views, nodes)}
}

// graphEdges draws every passed: constraint between placed nodes. Constraints
// sharing a job pair merge into one drawn edge carrying every resource:
// separate edges would overlap exactly, and only the topmost <title> is ever
// hoverable — hiding the rest.
func graphEdges(views []jobView, nodes []graphNode) []graphEdge {
	byNode := map[string]graphNode{}
	for _, node := range nodes {
		byNode[node.Name] = node
	}

	// Assigned, not var-declared: merging writes back through edges[at], and
	// nilaway cannot see that drawn only ever holds appended indices.
	edges := []graphEdge{}

	drawn := map[[2]string]int{}

	for _, view := range views {
		to := byNode[view.Name]

		for _, up := range view.Upstream {
			from, ok := byNode[up.Job]
			if !ok {
				continue
			}

			pair := [2]string{up.Job, view.Name}
			if at, merged := drawn[pair]; merged {
				edges[at].Resource += ", " + up.Resource

				continue
			}

			drawn[pair] = len(edges)

			edges = append(edges, graphEdge{
				From:     up.Job,
				To:       view.Name,
				Resource: up.Resource,
				Path:     edgePath(from, to),
			})
		}
	}

	return edges
}

// layerByLongestPath assigns each job the column one past its deepest
// upstream. A cycle — which config validation should never let through — is
// broken rather than followed, so a bad pipeline renders instead of hanging.
func layerByLongestPath(views []jobView, byName map[string]int) []int {
	layers := make([]int, len(views))
	state := make([]int, len(views)) // 0 unvisited, 1 in progress, 2 done

	var visit func(i int) int

	visit = func(i int) int {
		if state[i] == 2 {
			return layers[i]
		}

		if state[i] == 1 {
			return -1 // cycle: contribute nothing
		}

		state[i] = 1
		depth := 0

		for _, up := range views[i].Upstream {
			j, ok := byName[up.Job]
			if !ok {
				continue
			}

			if d := visit(j) + 1; d > depth {
				depth = d
			}
		}

		layers[i], state[i] = depth, 2

		return depth
	}

	for i := range views {
		visit(i)
	}

	return layers
}

// orderColumns runs one barycenter pass, left to right: each column sorts by
// the average vertical position of its upstreams in the columns before it,
// which is what keeps a diamond's edges from crossing.
func orderColumns(views []jobView, byName map[string]int, layers []int, columns map[int][]int, deepest int) {
	position := map[int]int{} // view index -> vertical slot in its column

	for _, column := range columns {
		for slot, i := range column {
			position[i] = slot
		}
	}

	for layer := 1; layer <= deepest; layer++ {
		column := columns[layer]

		weight := func(i int) float64 {
			sum, n := 0.0, 0

			for _, up := range views[i].Upstream {
				j, ok := byName[up.Job]
				if !ok || layers[j] >= layer {
					continue
				}

				sum += float64(position[j])
				n++
			}

			if n == 0 {
				return float64(position[i])
			}

			return sum / float64(n)
		}

		sort.SliceStable(column, func(a, b int) bool { return weight(column[a]) < weight(column[b]) })

		for slot, i := range column {
			position[i] = slot
		}
	}
}

// placeNodes turns columns into coordinates. Every node in a column shares
// that column's width, so the columns read as columns.
func placeNodes(views []jobView, columns map[int][]int, deepest int) []graphNode {
	nodes := make([]graphNode, len(views))

	x := dagMargin

	for layer := 0; layer <= deepest; layer++ {
		column := columns[layer]

		colW := dagMinWidth

		for _, i := range column {
			if w := nodeWidth(views[i]); w > colW {
				colW = w
			}
		}

		y := dagMargin

		for _, i := range column {
			view := views[i]
			word, glyph, class := nodeStatus(view)

			nodes[i] = graphNode{
				Name:        view.Name,
				Status:      word,
				Glyph:       glyph,
				StatusClass: class,
				Running:     view.HasRun && statusWord(view.Latest.Status) == "running",
				Layer:       layer,
				X:           x,
				Y:           y,
				W:           colW,
				H:           dagNodeH,
				TextX:       x + dagPadX,
				NameY:       y + dagNameY,
				StatusY:     y + dagStatusY,
			}

			y += dagNodeH + dagVGap
		}

		x += colW + dagHGap
	}

	return nodes
}

func nodeWidth(view jobView) int {
	word, glyph, _ := nodeStatus(view)

	chars := len(view.Name)
	if l := len(word) + len(glyph); l > chars {
		chars = l
	}

	if w := int(dagCharW*float64(chars)) + 2*dagPadX; w > dagMinWidth {
		return w
	}

	return dagMinWidth
}

// nodeStatus is the graph's reading of a job's latest run: the shared status
// word, its glyph, and the st-* class that colors the node.
func nodeStatus(view jobView) (word, glyph, class string) {
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

// edgePath draws one constraint as a cubic from the upstream node's right
// edge to the downstream node's left edge.
func edgePath(from, to graphNode) string {
	x1 := from.X + from.W
	y1 := from.Y + from.H/2
	x2 := to.X
	y2 := to.Y + to.H/2
	mid := (x1 + x2) / 2

	return fmt.Sprintf("M%d,%d C%d,%d %d,%d %d,%d", x1, y1, mid, y1, mid, y2, x2, y2)
}
