package machine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
function loop(body, opts) {
  return { __steps: "loop", body: body, opts: opts || {} };
}
// gate is a global-object property, not a lexical binding, so a machine that
// names a state "const gate = { human: ... }" shadows it without a redeclaration
// error — the common state name keeps working; only machines that actually call
// gate(...) reach the combinator.
globalThis.gate = function (name, opts) {
  return { __steps: "gate", name: name, opts: opts || {} };
};
const done = { __steps: "terminal", name: "done" };
const fail = { __steps: "terminal", name: "failed" };
function list(xs) {
  return (xs || []).map(function (x) { return "- " + x; }).join("\n");
}
// state(build) / state(name, build): the js-dsl closure form of a state. The
// build closure runs against a recorder whose setters mutate a config object
// ONLY when called — so the returned object is byte-identical to the equivalent
// literal and flows through the whole loader unchanged (structure stays data;
// the builder just assembles it dynamically). Like gate, it lives on globalThis
// so a machine may still name a state "const state = {...}" and shadow it —
// only actual state(...) calls reach the builder. An optional name is stamped
// non-enumerably as a hint the loader cross-checks against the states: key.
globalThis.state = function (a, b) {
  var name, build;
  if (typeof a === "function") {
    build = a;
  } else if (typeof a === "string" && typeof b === "function") {
    name = a;
    build = b;
  } else {
    throw new TypeError("state(build) or state(name, build): build must be a function");
  }

  var config = {};
  var proxy;
  function set(key) {
    return function (v) { config[key] = v; return proxy; };
  }
  var simpleKeys = [
    "forEach", "distill", "retry", "output", "input", "verdict",
    "prompt", "system", "model", "maxTurns", "maxOutputTokens", "maxInputTokens",
    "temperature", "reasoning", "structuredOutput", "toolChoice", "adopt",
    "history", "evidence", "action", "write", "content", "human", "timeout",
    "choices", "parallel", "concurrency", "onBranchFailure", "status",
  ];
  var setters = {};
  for (var i = 0; i < simpleKeys.length; i++) setters[simpleKeys[i]] = set(simpleKeys[i]);

  function toArray(a) {
    return (a.length === 1 && Array.isArray(a[0])) ? a[0] : Array.prototype.slice.call(a);
  }
  setters.tools = function () { config.tools = toArray(arguments); return proxy; };
  setters.tool = function (t) { (config.tools = config.tools || []).push(t); return proxy; };
  setters.events = function () { config.events = toArray(arguments); return proxy; };
  setters.memo = function (v) { config.memo = (arguments.length === 0) ? true : v; return proxy; };
  setters.terminal = function (v) { config.terminal = (arguments.length === 0) ? true : v; return proxy; };

  var valid = Object.keys(setters).sort().join(", ");
  proxy = new Proxy(setters, {
    get: function (target, prop) {
      if (typeof prop !== "string") return target[prop];
      if (prop === "then") return undefined; // never mistaken for a thenable
      if (Object.prototype.hasOwnProperty.call(target, prop)) return target[prop];
      throw new TypeError("unknown state builder method '" + prop + "' — valid: " + valid);
    },
  });

  build(proxy);
  if (name !== undefined) {
    Object.defineProperty(config, "__steps_state_name_hint", { value: name, enumerable: false });
  }
  return config;
};
// machine(name, build): the aasm-style whole-machine block. build(m) runs
// against a recorder whose verbs accumulate plain data — states (delegated to
// the state() builder), event/transition edges, and top-level config — then it
// RETURNS the standard config object the loader already consumes. The only new
// structure is the {__steps:"eventset"} flow node (wired in Go by wireEventSet).
// Events are first-class: m.event(name, {from, to, when}) owns its edge, fans in
// from many states (from: [a, b]), and — the ergonomic win — auto-declares the
// event on each non-human from-state so no separate events: list is needed. On
// globalThis for the same shadowing courtesy as gate/state.
globalThis.machine = function (name, build) {
  if (typeof name !== "string" || typeof build !== "function") {
    throw new TypeError("machine(name, build): name is a string, build is a function");
  }
  var states = {};   // insertion-ordered: key -> state config (identity preserved)
  var edges = [];     // ordered event/transition edges for the eventset node
  var cfg = {};       // top-level config keys

  function refKey(ref) {
    var k = ref && ref["__steps_state_name_hint"];
    if (typeof k !== "string") {
      throw new TypeError("machine(" + name + "): expected a state declared with m.state(...)");
    }
    return k;
  }
  function refs(from) { return Array.isArray(from) ? from : [from]; }

  var api = {};
  api.state = function (sname, buildOrConfig, opts) {
    if (typeof sname !== "string") throw new TypeError("m.state(name, ...): name must be a string");
    var config;
    if (typeof buildOrConfig === "function") {
      config = globalThis.state(sname, buildOrConfig);
    } else if (buildOrConfig && typeof buildOrConfig === "object") {
      config = buildOrConfig;
      if (config["__steps_state_name_hint"] === undefined) {
        Object.defineProperty(config, "__steps_state_name_hint", { value: sname, enumerable: false });
      }
    } else {
      throw new TypeError("m.state(" + sname + ", ...): second arg must be a build function or a config object");
    }
    states[sname] = config;
    if (opts && opts.initial) cfg.initial = sname;
    return config;
  };
  api.start = function (ref) { cfg.initial = refKey(ref); return m; };

  api.event = function (evName, spec) {
    spec = spec || {};
    var from = refs(spec.from);
    edges.push({ on: evName, when: spec.when, from: from, to: spec.to });
    for (var i = 0; i < from.length; i++) {
      var c = from[i];
      if (c && c.human !== undefined) continue; // gates route on resume events, not output.events
      var evs = c.events || (c.events = []);
      if (evs.indexOf(evName) === -1) evs.push(evName);
    }
    return m;
  };
  api.step = function (from, to) { edges.push({ from: refs(from), to: to }); return m; };
  api.always = api.step;
  api.guard = function (from, to, when) { edges.push({ when: when, from: refs(from), to: to }); return m; };
  api.catch = function (from, map) { edges.push({ catch: map, from: refs(from) }); return m; };
  api.timeout = function (gate, to) { edges.push({ timeout: true, from: refs(gate), to: to }); return m; };

  function setCfg(key) { return function (v) { cfg[key] = v; return m; }; }
  api.uses = setCfg("models"); api.models = api.uses;
  api.needs = setCfg("input"); api.input = api.needs;
  api.limit = setCfg("limits"); api.limits = api.limit;
  api.model = setCfg("model");
  api.defaults = setCfg("defaults");
  api.describe = setCfg("description");
  api.version = setCfg("version");
  api.webhook = setCfg("webhook");

  var valid = Object.keys(api).sort().join(", ");
  var m = new Proxy(api, {
    get: function (t, prop) {
      if (typeof prop !== "string") return t[prop];
      if (prop === "then") return undefined;
      if (Object.prototype.hasOwnProperty.call(t, prop)) return t[prop];
      throw new TypeError("unknown machine builder method '" + prop + "' — valid: " + valid);
    },
  });

  build(m);

  var out = { name: name, states: states };
  ["version", "description", "model", "models", "input", "defaults", "limits", "initial", "webhook"]
    .forEach(function (k) { if (cfg[k] !== undefined) out[k] = cfg[k]; });
  if (edges.length) out.flow = { __steps: "eventset", edges: edges };
  return out;
};
`

// stateNameProp carries the registered name on each state object so flow
// references resolve by identity regardless of goja wrapper details.
const stateNameProp = "__steps_state_name"

// stateNameHintProp carries the optional name argument passed to the state(...)
// builder. It is stamped non-enumerably (so it never reaches checkStateKeys) and
// the loader cross-checks it against the states: key to catch name/key drift.
const stateNameHintProp = "__steps_state_name_hint"

// compileFlow walks the exported flow value and wires the machine.
func (l *loader) compileFlow(m *Machine, flow goja.Value) error {
	entry, err := l.wireNode(m, flow, "")
	if err != nil {
		return err
	}
	if m.Initial == "" {
		m.Initial = entry
	}

	// Parallel branches are wired here — after the main flow places each fork
	// state (and its join successor), before the unwired-to-done fallthrough,
	// so a multi-state branch sub-flow chains internally instead of every state
	// falling straight to done.
	err = l.wireParallelBranches(m)
	if err != nil {
		return err
	}

	// Outgoing-edge rule: any non-terminal state left unwired flows to done.
	for _, s := range m.States {
		if !s.Terminal && len(s.Transitions) == 0 {
			s.Transitions = []Transition{{To: "done"}}
		}
	}
	return nil
}

// wireParallelBranches compiles each fork state's branch sub-flows. Each branch
// is a state ref or a pipe/branch/loop, wired with `done` as its successor so
// it terminates at the shared terminal; the resolved entry state and its label
// land in the fork's ParallelSpec.Branches. The "wired more than once" guard in
// wireNode enforces that branch closures are disjoint from the main flow and
// from each other — which is what lets a branch run as a hermetic sub-run.
func (l *loader) wireParallelBranches(m *Machine) error {
	for _, s := range m.States {
		if s.Parallel == nil {
			continue
		}
		pobj := l.parallelNodes[s.Name]
		if pobj == nil {
			return fmt.Errorf("parallel state %q has no branches", s.Name)
		}
		for _, label := range pobj.Keys() {
			entry, err := l.wireNode(m, pobj.Get(label), "done")
			if err != nil {
				return fmt.Errorf("parallel %q branch %q: %w", s.Name, label, err)
			}
			s.Parallel.Branches = append(s.Parallel.Branches, Branch{Label: label, Entry: entry})
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
			err := l.wireFallback(m, name, successor)
			if err != nil {
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
			return "", errors.New("pipe() needs at least one step")
		}
		// Wire back to front so each step knows its successor's entry.
		next := successor
		entry := ""
		for i := n - 1; i >= 0; i-- {
			e, err := l.wireNode(m, steps.Get(strconv.Itoa(i)), next)
			if err != nil {
				return "", err
			}
			next = e
			entry = e
		}
		return entry, nil

	case "branch":
		return l.wireBranch(m, obj, successor)

	case "loop":
		return l.wireLoop(m, obj, successor)

	case "gate":
		return l.wireGate(m, obj, successor)

	case "eventset":
		return l.wireEventSet(m, obj)

	case "when":
		return "", errors.New("when(...) must be completed with .to(target)")
	case "edge":
		return "", errors.New("when(...).to(...) is only valid as a branch edge value")
	}
	return "", fmt.Errorf("flow contains a value that is not a state, pipe, branch, loop, or terminal — got %s", v)
}

func (l *loader) wireBranch(m *Machine, obj *goja.Object, successor string) (string, error) {
	stateVal := obj.Get("state")
	kind, stateObj := l.flowKind(stateVal)
	if kind != "state" {
		return "", errors.New("branch(...) must start from a registered state")
	}
	name := l.stateName(stateObj)
	st := m.State(name)
	if st == nil {
		return "", fmt.Errorf("branch state %q is not registered in states", name)
	}
	if len(st.Transitions) > 0 {
		return "", fmt.Errorf("state %q is wired more than once — each state's outgoing edges live in exactly one place", name)
	}

	edges, ok := obj.Get("edges").(*goja.Object)
	if !ok {
		return "", fmt.Errorf("branch(%s, ...) needs an edges object or array", name)
	}

	var elseTarget string
	var hasElse bool
	var err error
	if edges.ClassName() == "Array" {
		elseTarget, hasElse, err = l.wireArrayEdges(m, st, name, edges)
	} else {
		elseTarget, hasElse, err = l.wireObjectEdges(m, st, name, edges)
	}
	if err != nil {
		return "", err
	}
	return name, l.finishBranch(st, name, elseTarget, hasElse, successor)
}

// wireArrayEdges wires a branch's array form: ordered guard-only edges —
// when(g).to(x) entries, with an optional bare target last as the else.
func (l *loader) wireArrayEdges(m *Machine, st *State, name string, edges *goja.Object) (elseTarget string, hasElse bool, err error) {
	n := int(edges.Get("length").ToInteger())
	for i := range n {
		entry := edges.Get(strconv.Itoa(i))
		if k, edgeObj := l.flowKind(entry); k == "edge" {
			guard := l.dyn(edgeObj.Get("when"))
			to, err := l.wireTarget(m, edgeObj.Get("to"), name, fmt.Sprintf("edge %d", i))
			if err != nil {
				return "", false, err
			}
			st.Transitions = append(st.Transitions, Transition{When: guard, To: to})
			continue
		}
		if i != n-1 {
			return "", false, fmt.Errorf("state %q: only the last array edge may be a bare target (the else)", name)
		}
		target, err := l.wireTarget(m, entry, name, "else")
		if err != nil {
			return "", false, err
		}
		elseTarget, hasElse = target, true
	}
	return elseTarget, hasElse, nil
}

// wireObjectEdges wires a branch's object form: else/catch/timeout keys plus
// event-name keys, each an edge or a bare target.
func (l *loader) wireObjectEdges(m *Machine, st *State, name string, edges *goja.Object) (elseTarget string, hasElse bool, err error) {
	for _, key := range edges.Keys() {
		val := edges.Get(key)
		switch key {
		case "else":
			target, err := l.wireTarget(m, val, name, "else")
			if err != nil {
				return "", false, err
			}
			elseTarget, hasElse = target, true

		case "catch":
			err := l.wireCatchEdges(m, st, name, val)
			if err != nil {
				return "", false, err
			}

		case "timeout":
			if st.Human == nil {
				return "", false, fmt.Errorf("state %q: timeout routes are only valid on human gates", name)
			}
			target, err := l.wireTarget(m, val, name, "timeout")
			if err != nil {
				return "", false, err
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
				return "", false, err
			}
			st.Transitions = append(st.Transitions, Transition{On: on, When: guard, To: to})
		}
	}
	return elseTarget, hasElse, nil
}

// wireCatchEdges wires a branch's catch: {errorClass: target} object.
func (l *loader) wireCatchEdges(m *Machine, st *State, name string, val goja.Value) error {
	catches, ok := val.(*goja.Object)
	if !ok {
		return fmt.Errorf("state %q: catch must be an object of {errorClass: target}", name)
	}
	for _, class := range catches.Keys() {
		target, err := l.wireTarget(m, catches.Get(class), name, "catch "+class)
		if err != nil {
			return err
		}
		st.Catch = append(st.Catch, CatchClause{Match: []string{class}, To: target})
	}
	return nil
}

// finishBranch wires the fallback: explicit else wins; mid-pipe, the
// successor is the else. Human gates need no else — their resume events are
// the complete alphabet (an unknown event is a runtime error, not a route).
func (l *loader) finishBranch(st *State, name, elseTarget string, hasElse bool, successor string) error {
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

// loopKeys are the valid loop(body, {...}) options.
var loopKeys = []string{"judge", "accept", "maxVisits", "then", "revise", "exhausted", "catch", "escalate"}

// wireLoop wires the bounded judge/revise cycle: the body falls through to
// the judge, and the judge's out-edges are exactly accept -> then, budget ->
// revise, fallback -> exhausted. Pure sugar over the same enforced
// Transitions an array-form branch builds by hand — the bound is a real JS
// guard over visits.<judge>, the one counter that is correct by construction
// (the judge runs exactly once per iteration; its out-edges ARE the loop).
func (l *loader) wireLoop(m *Machine, obj *goja.Object, successor string) (string, error) {
	opts, ok := obj.Get("opts").(*goja.Object)
	if !ok {
		return "", errors.New("loop(body, {...}) needs an options object")
	}
	for _, k := range opts.Keys() {
		if !contains(loopKeys, k) {
			return "", fmt.Errorf("loop: unknown option %q — valid options: %s", k, strings.Join(loopKeys, ", "))
		}
	}

	judge, st, err := l.resolveLoopJudge(m, opts)
	if err != nil {
		return "", err
	}

	// The body falls through to the judge.
	body := obj.Get("body")
	if bodyKind, _ := l.flowKind(body); bodyKind == "terminal" {
		return "", errors.New("loop: the body cannot be a terminal state")
	}
	entry, err := l.wireNode(m, body, judge)
	if err != nil {
		return "", err
	}
	if entry == judge {
		return "", fmt.Errorf("loop: judge %q cannot also be the body — write a self-judging state as an array-form branch", judge)
	}

	accept, err := l.resolveLoopAccept(opts, st, judge)
	if err != nil {
		return "", err
	}

	maxVisits, err := loopMaxVisits(opts, judge)
	if err != nil {
		return "", err
	}

	then, err := l.resolveLoopThen(m, opts, judge, successor)
	if err != nil {
		return "", err
	}

	// revise: where a rejected result re-enters. Defaults to the body's
	// entry; explicit for loops that re-enter upstream of the body.
	revise, err := l.resolveLoopFallback(m, opts, "revise", judge, entry)
	if err != nil {
		return "", err
	}

	// exhausted: budget spent without acceptance is a failure unless routed —
	// or, with escalate:, a synthesized human gate whose approve rejoins the
	// loop's accept route.
	exhausted, err := l.resolveLoopExhausted(m, opts, judge, then)
	if err != nil {
		return "", err
	}

	// The visits budget, synthesized as real JS: it dry-runs, contract-checks,
	// and --prints exactly like a hand-written guard. Judge names are
	// validated identifiers, so the assembled source is well-formed.
	guardSrc := fmt.Sprintf("({ visits }) => visits.%s < %d", judge, maxVisits)
	guardVal, err := l.rt.vm.RunString(guardSrc)
	if err != nil {
		return "", fmt.Errorf("loop(%s): building the visits guard: %w", judge, err)
	}

	st.Transitions = []Transition{
		{When: accept, To: then},
		{When: l.dyn(guardVal), To: revise},
		{To: exhausted},
	}

	// catch: the judge's catch edges, exactly as branch wires them.
	err = l.wireLoopCatch(m, opts, st, judge)
	if err != nil {
		return "", err
	}
	return entry, nil
}

// resolveLoopAccept resolves the loop's accept edge: the explicit accept:
// option, else the judge's own verdict:. Declaring both is an error — the
// acceptance test is declared once — and declaring neither is an error too.
func (l *loader) resolveLoopAccept(opts *goja.Object, st *State, judge string) (Dyn, error) {
	accept := opts.Get("accept")
	hasAccept := defined(accept)
	hasVerdict := !st.Verdict.IsZero()
	switch {
	case hasAccept && hasVerdict:
		return Dyn{}, fmt.Errorf("loop(%s): judge declares verdict: and the loop passes accept: — declare the acceptance test once", judge)
	case hasAccept:
		if _, isFn := goja.AssertFunction(accept); !isFn {
			return Dyn{}, fmt.Errorf("loop(%s): accept must be a function of scope returning a boolean", judge)
		}
		return l.dyn(accept), nil
	case hasVerdict:
		return st.Verdict, nil
	default:
		return Dyn{}, fmt.Errorf("loop(%s): no acceptance test — pass accept: or declare verdict: on the judge", judge)
	}
}

// resolveLoopExhausted resolves where a spent budget routes: the escalate:
// shorthand (a synthesized human gate), an explicit exhausted: target, or
// failed. escalate: fuses the commonest tail — "budget spent → ask a human →
// approve ships where accept would have, reject/timeout fail" — into the loop
// declaration itself: a string/function prompt, or {prompt, timeout}. The
// synthesized state is gate#<judge>_escalate, wired exactly as
// gate(name, {approve: <then>}) would be.
func (l *loader) resolveLoopExhausted(m *Machine, opts *goja.Object, judge, then string) (string, error) {
	esc := opts.Get("escalate")
	if !defined(esc) {
		return l.resolveLoopFallback(m, opts, "exhausted", judge, "failed")
	}
	if defined(opts.Get("exhausted")) {
		return "", fmt.Errorf("loop(%s): pass escalate: or exhausted:, not both", judge)
	}

	prompt, timeout, err := l.parseLoopEscalate(esc, judge)
	if err != nil {
		return "", err
	}

	name := "gate#" + judge + "_escalate"
	if m.State(name) != nil {
		return "", fmt.Errorf("loop(%s): escalate gate %q already exists", judge, name)
	}
	st := &State{Name: name, Gate: true, Human: &HumanSpec{Prompt: l.dyn(prompt), Timeout: timeout}}
	l.applyGateApprove(st, then)
	if timeout > 0 {
		st.Human.OnTimeout = "failed"
	}
	m.States = append(m.States, st)
	m.buildIndex()
	return name, nil
}

// parseLoopEscalate parses the escalate: value — a bare prompt (string or
// function), or {prompt, timeout} — into the gate's prompt and timeout.
func (l *loader) parseLoopEscalate(esc goja.Value, judge string) (goja.Value, time.Duration, error) {
	if _, isFn := goja.AssertFunction(esc); isFn {
		return esc, 0, nil
	}
	if _, isStr := esc.Export().(string); isStr {
		return esc, 0, nil
	}
	o := l.obj(esc)
	if o == nil {
		return nil, 0, fmt.Errorf("loop(%s): escalate must be a prompt (string or function) or {prompt, timeout}", judge)
	}
	for _, k := range o.Keys() {
		if k != "prompt" && k != "timeout" {
			return nil, 0, fmt.Errorf("loop(%s): escalate: unknown key %q — valid: prompt, timeout", judge, k)
		}
	}
	prompt := o.Get("prompt")
	if !defined(prompt) {
		return nil, 0, fmt.Errorf("loop(%s): escalate needs a prompt", judge)
	}
	timeout, err := duration(o.Get("timeout"), fmt.Sprintf("loop(%s).escalate.timeout", judge))
	if err != nil {
		return nil, 0, err
	}
	return prompt, timeout, nil
}

// resolveLoopFallback resolves an optional loop target (revise/exhausted):
// wired to its declared value, or the given default when absent.
func (l *loader) resolveLoopFallback(m *Machine, opts *goja.Object, key, judge, def string) (string, error) {
	v := opts.Get(key)
	if !defined(v) {
		return def, nil
	}
	return l.wireTarget(m, v, judge, key)
}

// wireLoopCatch wires the judge's catch: {errorClass: target} object,
// exactly as branch wires it.
func (l *loader) wireLoopCatch(m *Machine, opts *goja.Object, st *State, judge string) error {
	v := opts.Get("catch")
	if !defined(v) {
		return nil
	}
	catches, isObj := v.(*goja.Object)
	if !isObj {
		return fmt.Errorf("loop(%s): catch must be an object of {errorClass: target}", judge)
	}
	for _, class := range catches.Keys() {
		target, err := l.wireTarget(m, catches.Get(class), judge, "catch "+class)
		if err != nil {
			return err
		}
		st.Catch = append(st.Catch, CatchClause{Match: []string{class}, To: target})
	}
	return nil
}

// resolveLoopJudge resolves and validates a loop's judge option: a
// registered, non-terminal, non-human, not-yet-wired state whose out-edges
// the loop will own.
func (l *loader) resolveLoopJudge(m *Machine, opts *goja.Object) (judge string, st *State, err error) {
	kind, judgeObj := l.flowKind(opts.Get("judge"))
	if kind != "state" {
		return "", nil, errors.New("loop: judge must be a registered state")
	}
	judge = l.stateName(judgeObj)
	st = m.State(judge)
	if st == nil {
		return "", nil, fmt.Errorf("loop: judge %q is not registered in states", judge)
	}
	if st.Terminal {
		return "", nil, fmt.Errorf("loop: judge %q is terminal and cannot route the loop", judge)
	}
	if st.Human != nil {
		return "", nil, fmt.Errorf("loop: judge %q is a human gate — gates route on resume events; use branch", judge)
	}
	if len(st.Transitions) > 0 {
		return "", nil, fmt.Errorf("state %q is wired more than once — each state's outgoing edges live in exactly one place", judge)
	}
	return judge, st, nil
}

// loopMaxVisits parses and validates the required maxVisits option — the
// declared bound is the combinator's point, so it has no default.
func loopMaxVisits(opts *goja.Object, judge string) (int, error) {
	mv := opts.Get("maxVisits")
	if !defined(mv) {
		return 0, fmt.Errorf("loop(%s): maxVisits is required — the bounded budget is the point of a loop", judge)
	}
	switch mv.Export().(type) {
	case int64, float64:
	default:
		return 0, fmt.Errorf("loop(%s): maxVisits must be a number, got %s", judge, mv)
	}
	maxVisits := int(mv.ToInteger())
	if maxVisits < 1 {
		return 0, fmt.Errorf("loop(%s): maxVisits must be >= 1, got %s", judge, mv)
	}
	return maxVisits, nil
}

// resolveLoopThen resolves the loop's then: target, explicit XOR the pipe
// successor — the same rule as branch's else.
func (l *loader) resolveLoopThen(m *Machine, opts *goja.Object, judge, successor string) (string, error) {
	thenVal := opts.Get("then")
	switch {
	case defined(thenVal) && successor != "":
		return "", fmt.Errorf("loop(%s): loop has a then: AND the pipe continues after it — move the continuation into then: or drop it", judge)
	case defined(thenVal):
		return l.wireTarget(m, thenVal, judge, "then")
	case successor != "":
		return successor, nil
	default:
		return "", fmt.Errorf("loop(%s): loop at the end of a pipe needs a then", judge)
	}
}

// gateKeys are the valid gate(name, {...}) options.
var gateKeys = []string{"prompt", "approve", "choices", "timeout", "onTimeout"}

// wireGate synthesizes a human escalation state (`gate#<name>`) and its branch
// tail from a prompt + a choice→target map. It is the same "sugar compiles to
// states" move as distill's `owner#key`: the `#` keeps the name collision-free
// and non-destructurable, and the synthesized state flows through defaults,
// validation, journal, and resume exactly like a hand-written gate.
func (l *loader) wireGate(m *Machine, obj *goja.Object, successor string) (string, error) {
	// Reuse: a gate node referenced from more than one target position resolves
	// to the same synthesized state. A gate places itself, so a pipe successor
	// (mid-pipe re-wiring) is the double-wire error.
	if defined(obj.Get(stateNameProp)) {
		name := l.stateName(obj)
		if successor != "" {
			return "", fmt.Errorf("gate %q is wired more than once — a gate routes on its own events; reference it once", name)
		}
		return name, nil
	}

	rawName := str(obj.Get("name"))
	if !isIdentifier(rawName) {
		return "", fmt.Errorf("gate(%q): name must be a valid identifier (letters, digits, _)", rawName)
	}
	stateName := "gate#" + rawName
	if m.State(stateName) != nil {
		return "", fmt.Errorf("gate %q is declared twice", rawName)
	}
	opts, ok := obj.Get("opts").(*goja.Object)
	if !ok {
		return "", fmt.Errorf("gate(%q): needs an options object", rawName)
	}
	for _, k := range opts.Keys() {
		if !contains(gateKeys, k) {
			return "", fmt.Errorf("gate(%q): unknown option %q — valid: %s", rawName, k, strings.Join(gateKeys, ", "))
		}
	}

	prompt := opts.Get("prompt")
	if !defined(prompt) {
		return "", fmt.Errorf("gate(%q): needs a prompt", rawName)
	}
	timeout, err := duration(opts.Get("timeout"), fmt.Sprintf("gate(%q).timeout", rawName))
	if err != nil {
		return "", err
	}

	// Register the state before wiring targets — they may reference it or be
	// nested subtrees that need it in the index.
	st := &State{
		Name:  stateName,
		Gate:  true,
		Human: &HumanSpec{Prompt: l.dyn(prompt), Timeout: timeout},
	}
	m.States = append(m.States, st)
	m.buildIndex()
	_ = obj.DefineDataProperty(stateNameProp, l.rt.vm.ToValue(stateName), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE)

	err = l.wireGateEdges(m, st, rawName, opts, successor)
	if err != nil {
		return "", err
	}
	err = l.wireGateTimeout(m, st, rawName, opts, timeout)
	if err != nil {
		return "", err
	}
	return stateName, nil
}

// wireGateEdges resolves the choice→event transitions and the ChoiceSpec: the
// approve: shorthand (approved → target, synthesized rejected → fail), the full
// choices: map, or — mid-pipe with neither — approve defaults to the successor.
func (l *loader) wireGateEdges(m *Machine, st *State, name string, opts *goja.Object, successor string) error {
	approve := opts.Get("approve")
	choices := opts.Get("choices")
	switch {
	case defined(approve) && defined(choices):
		return fmt.Errorf("gate(%q): pass approve: or choices:, not both", name)
	case defined(approve):
		if successor != "" {
			return fmt.Errorf("gate(%q): has approve and the pipe continues after it — move the continuation into approve", name)
		}
		to, err := l.wireTarget(m, approve, st.Name, "approve")
		if err != nil {
			return err
		}
		l.applyGateApprove(st, to)
	case defined(choices):
		if successor != "" {
			return fmt.Errorf("gate(%q): has choices: AND the pipe continues after it — a gate routes on its own events", name)
		}
		return l.wireGateChoices(m, st, name, choices)
	default:
		if successor == "" {
			return fmt.Errorf("gate(%q): needs approve: or choices: (or a pipe to fall through to)", name)
		}
		l.applyGateApprove(st, successor)
	}
	return nil
}

// applyGateApprove wires the approve: shorthand: approved → target, rejected →
// failed, with a synthesized single-choice alphabet so the gate renders.
func (l *loader) applyGateApprove(st *State, approveTarget string) {
	st.Transitions = []Transition{
		{On: "approved", To: approveTarget},
		{On: "rejected", To: "failed"},
	}
	st.Human.Choices = &ChoiceSpec{Kind: "single", Options: []ChoiceOption{
		{Event: "approved", Label: "Approve"},
		{Event: "rejected", Label: "Reject (fail the run)"},
	}}
}

// wireGateChoices wires the full choices: {event: target | {to, label}} map.
func (l *loader) wireGateChoices(m *Machine, st *State, name string, v goja.Value) error {
	o := l.obj(v)
	if o == nil {
		return fmt.Errorf("gate(%q): choices must be an object of {event: target | {to, label}}", name)
	}
	keys := o.Keys()
	if len(keys) == 0 {
		return fmt.Errorf("gate(%q): choices must declare at least one option", name)
	}
	choice := &ChoiceSpec{Kind: "single"}
	for _, ev := range keys {
		val := o.Get(ev)
		target, label := val, ev
		// {to, label} form: a plain object (not a flow node) carrying `to`.
		if k, vo := l.flowKind(val); k == "" && vo != nil && defined(vo.Get("to")) {
			target = vo.Get("to")
			if defined(vo.Get("label")) {
				label = str(vo.Get("label"))
			}
		}
		to, err := l.wireTarget(m, target, st.Name, "choice "+ev)
		if err != nil {
			return err
		}
		st.Transitions = append(st.Transitions, Transition{On: ev, To: to})
		choice.Options = append(choice.Options, ChoiceOption{Event: ev, Label: label})
	}
	st.Human.Choices = choice
	return nil
}

// wireGateTimeout wires the timeout route: an explicit onTimeout: target, or —
// when a timeout duration is set — a synthesized route to failed.
func (l *loader) wireGateTimeout(m *Machine, st *State, name string, opts *goja.Object, timeout time.Duration) error {
	onTimeout := opts.Get("onTimeout")
	switch {
	case defined(onTimeout):
		if timeout == 0 {
			return fmt.Errorf("gate(%q): onTimeout is set but timeout is not", name)
		}
		to, err := l.wireTarget(m, onTimeout, st.Name, "onTimeout")
		if err != nil {
			return err
		}
		st.Human.OnTimeout = to
	case timeout > 0:
		st.Human.OnTimeout = "failed"
	}
	return nil
}

// wireEventSet wires the aasm-style machine() block: an ordered list of edges,
// each owning a (from-states -> to) transition, a catch map, or a gate timeout.
// It appends transitions to each from-state DIRECTLY and — unlike wireFallback /
// wireBranch — deliberately does NOT enforce the "wired once" guard: fan-in means
// one event edge touches many states, and a state may collect several edges. The
// eventset is still the single place a state's out-edges are authored, so the
// invariant holds. Fallback presence/order, event-declared, and reachability are
// left to validate.go exactly as the flow combinators leave them.
func (l *loader) wireEventSet(m *Machine, obj *goja.Object) (string, error) {
	edges, ok := obj.Get("edges").(*goja.Object)
	if !ok || edges.ClassName() != "Array" {
		return "", errors.New("machine(): eventset has no edges")
	}
	n := int(edges.Get("length").ToInteger())
	for i := range n {
		edge := l.obj(edges.Get(strconv.Itoa(i)))
		if edge == nil {
			return "", fmt.Errorf("machine(): edge %d is not an object", i)
		}
		fromNames, err := l.eventFromStates(edge, i)
		if err != nil {
			return "", err
		}
		err = l.wireEventEdge(m, edge, fromNames)
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

// eventFromStates resolves an edge's `from` (a state ref or an array of them,
// normalized to an array by the builder) to registered state names.
func (l *loader) eventFromStates(edge *goja.Object, i int) ([]string, error) {
	fromArr, ok := edge.Get("from").(*goja.Object)
	if !ok || fromArr.ClassName() != "Array" {
		return nil, fmt.Errorf("machine(): edge %d has no from state(s)", i)
	}
	n := int(fromArr.Get("length").ToInteger())
	names := make([]string, 0, n)
	for j := range n {
		kind, fobj := l.flowKind(fromArr.Get(strconv.Itoa(j)))
		if kind != "state" {
			return nil, fmt.Errorf("machine(): edge %d from[%d] is not a registered state", i, j)
		}
		names = append(names, l.stateName(fobj))
	}
	return names, nil
}

// wireEventEdge lowers one edge onto every from-state: a catch map -> Catch, a
// gate timeout -> Human.OnTimeout, or a (guarded) event -> Transition.
func (l *loader) wireEventEdge(m *Machine, edge *goja.Object, fromNames []string) error {
	switch {
	case defined(edge.Get("catch")):
		catches := l.obj(edge.Get("catch"))
		if catches == nil {
			return errors.New("machine(): catch must be an object of {errorClass: target}")
		}
		for _, from := range fromNames {
			st := m.State(from)
			for _, class := range catches.Keys() {
				to, err := l.wireTarget(m, catches.Get(class), from, "catch "+class)
				if err != nil {
					return err
				}
				st.Catch = append(st.Catch, CatchClause{Match: []string{class}, To: to})
			}
		}
		return nil

	case boolean(edge.Get("timeout")):
		to, err := l.wireTarget(m, edge.Get("to"), fromNames[0], "timeout")
		if err != nil {
			return err
		}
		for _, from := range fromNames {
			st := m.State(from)
			if st.Human == nil {
				return fmt.Errorf("machine(): timeout route on %q, which is not a human gate", from)
			}
			st.Human.OnTimeout = to
		}
		return nil

	default:
		var on string
		if defined(edge.Get("on")) {
			on = str(edge.Get("on"))
		}
		var guard Dyn
		if defined(edge.Get("when")) {
			guard = l.dyn(edge.Get("when"))
		}
		to, err := l.wireTarget(m, edge.Get("to"), fromNames[0], "on "+on)
		if err != nil {
			return err
		}
		for _, from := range fromNames {
			st := m.State(from)
			st.Transitions = append(st.Transitions, Transition{On: on, When: guard, To: to})
		}
		return nil
	}
}

// wireTarget resolves an edge target (state ref, terminal, nested pipe,
// branch, or loop) and returns the entry state name.
func (l *loader) wireTarget(m *Machine, v goja.Value, from, edge string) (string, error) {
	kind, obj := l.flowKind(v)
	switch kind {
	case "state":
		return l.stateName(obj), nil
	case "terminal":
		return str(obj.Get("name")), nil
	case "pipe", "branch", "loop", "gate":
		return l.wireNode(m, v, "")
	}
	return "", fmt.Errorf("state %q edge %s: target must be a registered state, done/fail, pipe(...), branch(...), loop(...), or gate(...)", from, edge)
}

func (l *loader) wireFallback(m *Machine, name, to string) error {
	st := m.State(name)
	if st == nil {
		return fmt.Errorf("flow references %q, which is not registered in states", name)
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
