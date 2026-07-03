package machine

import (
	"fmt"

	"github.com/dop251/goja"
)

// The flow combinators express the graph in one visible expression — the
// owner's `a |> (b || c)` sketch in legal JavaScript. They are sugar over
// the SAME enforced structures: the walker compiles the flow tree into
// State.Transitions / State.Catch / human timeout routes, and every
// existing validation (reachability, terminal proofs, fallback presence,
// event declarations) runs on the result. Combinators can never bypass the
// machine; they only build it.

// flowBootstrapJS defines the combinators in the machine's own runtime so
// edge maps keep JS insertion order and state references keep identity.
const flowBootstrapJS = `
function pipe() {
  return { __steps: "pipe", steps: Array.prototype.slice.call(arguments) };
}
function branch(state, edges) {
  return { __steps: "branch", state: state, edges: edges || {} };
}
function when(fn) {
  return {
    __steps: "when",
    fn: fn,
    to: function (target) { return { __steps: "edge", when: fn, to: target }; },
  };
}
const done = { __steps: "terminal", name: "done" };
const fail = { __steps: "terminal", name: "failed" };
function list(xs) {
  return (xs || []).map(function (x) { return "- " + x; }).join("\n");
}
`

// stateNameProp carries the registered name on each state object so flow
// references resolve by identity regardless of goja wrapper details.
const stateNameProp = "__steps_state_name"

// compileFlow walks the exported flow value and wires the machine.
func (l *loader) compileFlow(m *Machine, flow goja.Value) error {
	entry, err := l.wireNode(m, flow, "")
	if err != nil {
		return err
	}
	if m.Initial == "" {
		m.Initial = entry
	}

	// Outgoing-edge rule: any non-terminal state left unwired flows to done.
	for _, s := range m.States {
		if !s.Terminal && len(s.Transitions) == 0 {
			s.Transitions = []Transition{{To: "done"}}
		}
	}
	return nil
}

// wireNode wires a flow node and returns its entry state name. successor is
// the state a mid-pipe node falls through to ("" at pipe end).
func (l *loader) wireNode(m *Machine, v goja.Value, successor string) (string, error) {
	kind, obj := l.flowKind(v)
	switch kind {
	case "state":
		name := l.stateName(obj)
		if successor != "" {
			if err := l.wireFallback(m, name, successor); err != nil {
				return "", err
			}
		}
		return name, nil

	case "terminal":
		return str(obj.Get("name")), nil

	case "pipe":
		steps := obj.Get("steps").(*goja.Object)
		n := int(steps.Get("length").ToInteger())
		if n == 0 {
			return "", fmt.Errorf("pipe() needs at least one step")
		}
		// Wire back to front so each step knows its successor's entry.
		next := successor
		entry := ""
		for i := n - 1; i >= 0; i-- {
			e, err := l.wireNode(m, steps.Get(fmt.Sprintf("%d", i)), next)
			if err != nil {
				return "", err
			}
			next = e
			entry = e
		}
		return entry, nil

	case "branch":
		return l.wireBranch(m, obj, successor)

	case "when":
		return "", fmt.Errorf("when(...) must be completed with .to(target)")
	case "edge":
		return "", fmt.Errorf("when(...).to(...) is only valid as a branch edge value")
	}
	return "", fmt.Errorf("flow contains a value that is not a state, pipe, branch, or terminal — got %s", v)
}

func (l *loader) wireBranch(m *Machine, obj *goja.Object, successor string) (string, error) {
	stateVal := obj.Get("state")
	kind, stateObj := l.flowKind(stateVal)
	if kind != "state" {
		return "", fmt.Errorf("branch(...) must start from a registered state")
	}
	name := l.stateName(stateObj)
	st := m.State(name)
	if st == nil {
		return "", fmt.Errorf("branch state %q is not registered in states:", name)
	}
	if len(st.Transitions) > 0 {
		return "", fmt.Errorf("state %q is wired more than once — each state's outgoing edges live in exactly one place", name)
	}

	edges, ok := obj.Get("edges").(*goja.Object)
	if !ok {
		return "", fmt.Errorf("branch(%s, ...) needs an edges object or array", name)
	}

	var elseTarget string
	hasElse := false

	// Array form: ordered guard-only edges — when(g).to(x) entries, with an
	// optional bare target last as the else.
	if edges.ClassName() == "Array" {
		n := int(edges.Get("length").ToInteger())
		for i := 0; i < n; i++ {
			entry := edges.Get(fmt.Sprintf("%d", i))
			if k, edgeObj := l.flowKind(entry); k == "edge" {
				guard := l.dyn(edgeObj.Get("when"))
				to, err := l.wireTarget(m, edgeObj.Get("to"), name, fmt.Sprintf("edge %d", i))
				if err != nil {
					return "", err
				}
				st.Transitions = append(st.Transitions, Transition{When: guard, To: to})
				continue
			}
			if i != n-1 {
				return "", fmt.Errorf("state %q: only the last array edge may be a bare target (the else)", name)
			}
			target, err := l.wireTarget(m, entry, name, "else")
			if err != nil {
				return "", err
			}
			elseTarget, hasElse = target, true
		}
		return name, l.finishBranch(m, st, name, elseTarget, hasElse, successor)
	}

	for _, key := range edges.Keys() {
		val := edges.Get(key)
		switch key {
		case "else":
			target, err := l.wireTarget(m, val, name, "else")
			if err != nil {
				return "", err
			}
			elseTarget, hasElse = target, true

		case "catch":
			catches, ok := val.(*goja.Object)
			if !ok {
				return "", fmt.Errorf("state %q: catch must be an object of {errorClass: target}", name)
			}
			for _, class := range catches.Keys() {
				target, err := l.wireTarget(m, catches.Get(class), name, "catch "+class)
				if err != nil {
					return "", err
				}
				st.Catch = append(st.Catch, CatchClause{Match: []string{class}, To: target})
			}

		case "timeout":
			if st.Human == nil {
				return "", fmt.Errorf("state %q: timeout routes are only valid on human gates", name)
			}
			target, err := l.wireTarget(m, val, name, "timeout")
			if err != nil {
				return "", err
			}
			st.Human.OnTimeout = target

		default: // an event edge
			on := key
			var guard Dyn
			target := val
			if k, edgeObj := l.flowKind(val); k == "edge" {
				guard = l.dyn(edgeObj.Get("when"))
				target = edgeObj.Get("to")
			}
			to, err := l.wireTarget(m, target, name, "on "+on)
			if err != nil {
				return "", err
			}
			st.Transitions = append(st.Transitions, Transition{On: on, When: guard, To: to})
		}
	}

	return name, l.finishBranch(m, st, name, elseTarget, hasElse, successor)
}

// finishBranch wires the fallback: explicit else wins; mid-pipe, the
// successor is the else. Human gates need no else — their resume events are
// the complete alphabet (an unknown event is a runtime error, not a route).
func (l *loader) finishBranch(m *Machine, st *State, name, elseTarget string, hasElse bool, successor string) error {
	switch {
	case hasElse && successor != "":
		return fmt.Errorf("state %q: branch has an else AND the pipe continues after it — move the continuation inside the branch or drop the else", name)
	case hasElse:
		st.Transitions = append(st.Transitions, Transition{To: elseTarget})
	case successor != "":
		st.Transitions = append(st.Transitions, Transition{To: successor})
	case st.Human == nil:
		return fmt.Errorf("state %q: branch at the end of a pipe needs an else edge", name)
	}
	return nil
}

// wireTarget resolves an edge target (state ref, terminal, nested pipe or
// branch) and returns the entry state name.
func (l *loader) wireTarget(m *Machine, v goja.Value, from, edge string) (string, error) {
	kind, obj := l.flowKind(v)
	switch kind {
	case "state":
		return l.stateName(obj), nil
	case "terminal":
		return str(obj.Get("name")), nil
	case "pipe", "branch":
		return l.wireNode(m, v, "")
	}
	return "", fmt.Errorf("state %q edge %s: target must be a registered state, done/fail, pipe(...), or branch(...)", from, edge)
}

func (l *loader) wireFallback(m *Machine, name, to string) error {
	st := m.State(name)
	if st == nil {
		return fmt.Errorf("flow references %q, which is not registered in states:", name)
	}
	if st.Terminal {
		return fmt.Errorf("terminal state %q cannot have outgoing wiring", name)
	}
	if len(st.Transitions) > 0 {
		return fmt.Errorf("state %q is wired more than once — each state's outgoing edges live in exactly one place", name)
	}
	st.Transitions = []Transition{{To: to}}
	return nil
}

// flowKind classifies a flow node: a combinator (tagged), a registered
// state object (carries the hidden name), or something invalid.
func (l *loader) flowKind(v goja.Value) (string, *goja.Object) {
	obj := l.obj(v)
	if obj == nil {
		return "", nil
	}
	if tag := obj.Get("__steps"); defined(tag) {
		return tag.String(), obj
	}
	if name := obj.Get(stateNameProp); defined(name) {
		return "state", obj
	}
	return "", obj
}

func (l *loader) stateName(obj *goja.Object) string {
	return str(obj.Get(stateNameProp))
}
