package machine

import "strings"

// Graph is the machine's topology as plain data: nodes (states) and edges
// (every way to leave a state — transitions, catch routes, and a human gate's
// timeout). It is presentation-free — no coordinates, no colors — so the same
// view feeds a web diagram, a `steps graph` CLI, or docs. It mirrors the edge
// rule the engine enforces (see edges() in validate.go); keeping it here keeps
// that rule in one place.

// EdgeKind classifies why one state routes to another.
type EdgeKind string

const (
	EdgeNormal   EdgeKind = "normal"   // a conditional transition (on and/or when)
	EdgeFallback EdgeKind = "fallback" // an unconditional transition (the else / fallthrough)
	EdgeCatch    EdgeKind = "catch"    // a failure route (catch: {class: target})
	EdgeTimeout  EdgeKind = "timeout"  // a human gate's expiry route
)

// GraphNode is one state in the topology.
type GraphNode struct {
	Name     string
	Kind     string // HandlerKind(): agent | action | human | terminal
	Terminal bool
	Status   string // "" | "failed" (terminal states only)
	Initial  bool
	Model    string // agent states: the model ref/alias (Dyn.Display())
}

// GraphEdge is one directed route between states.
type GraphEdge struct {
	From, To string
	Kind     EdgeKind
	On       string // Transition.On (event that selects this edge)
	Guard    string // Transition.When source, or catch match classes
	Label    string // short precomputed display label
}

// GraphView is the whole topology.
type GraphView struct {
	Name    string
	Initial string
	Nodes   []GraphNode
	Edges   []GraphEdge
}

// Graph extracts the machine's topology. Implicit distill states (name#key)
// are hidden: their chain is collapsed so a predecessor edge lands on the
// consumer it feeds, never on a hidden node. Every visible edge therefore has
// a visible endpoint.
func (m *Machine) Graph() GraphView {
	gv := GraphView{Name: m.Name, Initial: m.Initial}

	for _, s := range m.States {
		if s.IsDistill() {
			continue
		}
		n := GraphNode{
			Name:     s.Name,
			Kind:     s.HandlerKind(),
			Terminal: s.Terminal,
			Status:   s.Status,
			Initial:  s.Name == m.Initial,
		}
		if s.Agent != nil {
			n.Model = s.Agent.Model.Display()
		}
		gv.Nodes = append(gv.Nodes, n)
	}

	for _, s := range m.States {
		if s.IsDistill() {
			continue
		}
		for _, t := range s.Transitions {
			kind := EdgeNormal
			if t.Fallback() {
				kind = EdgeFallback
			}
			guard := t.When.Display()
			gv.Edges = append(gv.Edges, GraphEdge{
				From:  s.Name,
				To:    m.resolveGraphTarget(t.To),
				Kind:  kind,
				On:    t.On,
				Guard: guard,
				Label: edgeLabel(t.On, guard),
			})
		}
		for _, c := range s.Catch {
			classes := strings.Join(c.Match, ",")
			gv.Edges = append(gv.Edges, GraphEdge{
				From:  s.Name,
				To:    m.resolveGraphTarget(c.To),
				Kind:  EdgeCatch,
				Guard: classes,
				Label: "catch:" + classes,
			})
		}
		if s.Human != nil && s.Human.OnTimeout != "" {
			gv.Edges = append(gv.Edges, GraphEdge{
				From:  s.Name,
				To:    m.resolveGraphTarget(s.Human.OnTimeout),
				Kind:  EdgeTimeout,
				Label: "timeout",
			})
		}
	}
	return gv
}

// resolveGraphTarget follows a distill chain to the visible consumer it feeds.
// Distill states have exactly one outgoing transition, so this is a straight
// walk; the seen guard is belt-and-suspenders against a malformed chain.
func (m *Machine) resolveGraphTarget(name string) string {
	seen := map[string]bool{}
	for {
		s := m.State(name)
		if s == nil || !s.IsDistill() {
			return name
		}
		if seen[name] || len(s.Transitions) == 0 {
			return name
		}
		seen[name] = true
		name = s.Transitions[0].To
	}
}

// edgeLabel renders a transition's condition compactly: on:<event> and/or
// when:<guard>. A bare fallback (neither) renders blank — the arrow says it.
func edgeLabel(on, guard string) string {
	var parts []string
	if on != "" {
		parts = append(parts, "on:"+on)
	}
	if guard != "" {
		parts = append(parts, "when:"+truncateLabel(guardBody(guard), 24))
	}
	return strings.Join(parts, " ")
}

// guardBody strips the leading arrow-function boilerplate ("({ output }) =>")
// from a guard source so the label shows the predicate that actually matters.
func guardBody(src string) string {
	if i := strings.Index(src, "=>"); i >= 0 {
		if body := strings.TrimSpace(src[i+2:]); body != "" {
			return body
		}
	}
	return strings.TrimSpace(src)
}

// truncateLabel clips a guard source for display, appending an ellipsis.
func truncateLabel(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
