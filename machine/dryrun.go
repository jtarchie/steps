package machine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dop251/goja"
)

// DryRun calls every function in the machine once against stub scopes derived
// from the declared schemas — before any token is spent. Accessing a field
// that cannot exist is a fatal error naming the function, the field, and the
// available fields. Other runtime errors become warnings (stub values cannot
// prove them real).
//
// Strictness follows knowledge: agent outputs have declared schemas and stub
// strictly; action outputs and foreach items are opaque and stub permissively
// (an "anything" value that tolerates any access).
func DryRun(m *Machine) (fatals []error, warnings []string) {
	if m.rt == nil {
		return nil, nil
	}
	if err := m.rt.installStubs(); err != nil {
		return []error{fmt.Errorf("installing dry-run stubs: %w", err)}, nil
	}

	record := func(state, site string, err error) {
		if err == nil {
			return
		}
		msg := fmt.Sprintf("state %q %s: %v", state, site, err)
		if strings.Contains(err.Error(), "unknown field") {
			fatals = append(fatals, fmt.Errorf("%s", msg))
			return
		}
		warnings = append(warnings, msg)
	}

	for _, s := range m.States {
		if s.Terminal {
			continue
		}
		base := m.stubScope(s)

		itemScope := base
		if f := s.ForEach; f != nil {
			record(s.Name, "foreach.over", dryCall(f.Over, base))
			itemScope = cloneScope(base)
			itemScope[f.As] = anyMarker()
			itemScope["index"] = 0
			itemScope["total"] = 1
		}

		if a := s.Agent; a != nil {
			record(s.Name, "model", dryCall(a.Model, itemScope))
			record(s.Name, "system", dryCall(a.System, itemScope))
			record(s.Name, "prompt", dryCall(a.Prompt, itemScope))
			toolScope := cloneScope(itemScope)
			toolScope["args"] = anyMarker()
			calls := map[string]any{}
			for _, tr := range a.Tools {
				calls[tr.Name] = 0
			}
			toolScope["calls"] = calls
			toolScope["turn"] = 1
			for _, tr := range a.Tools {
				record(s.Name, "tool "+tr.Name+" when", dryCall(tr.When, toolScope))
				record(s.Name, "tool "+tr.Name+" args", dryCall(tr.Args, toolScope))
			}
		}
		if h := s.Human; h != nil {
			record(s.Name, "human.prompt", dryCall(h.Prompt, itemScope))
		}
		record(s.Name, "input", dryInputs(s.Input, itemScope))
		for i, t := range s.Transitions {
			// Transition guards also see the state's own output.
			guardScope := cloneScope(itemScope)
			guardScope["output"] = m.outputStub(s)
			guardScope["event"] = ""
			record(s.Name, fmt.Sprintf("transitions[%d].when", i), dryCall(t.When, guardScope))
		}
	}
	return fatals, warnings
}

func dryCall(d Dyn, scope map[string]any) error {
	if !d.IsFn() {
		return nil
	}
	stubbed, err := d.rt.stubScope(scope)
	if err != nil {
		return err
	}
	_, err = d.rt.call(d.fn, d.Src, stubbed)
	return err
}

func dryInputs(d Dyn, scope map[string]any) error {
	if d.IsFn() {
		return dryCall(d, scope)
	}
	if m, ok := d.Static.(map[string]any); ok {
		for k, v := range m {
			if nested, ok := v.(Dyn); ok {
				if err := dryCall(nested, scope); err != nil {
					return fmt.Errorf("%s: %w", k, err)
				}
			}
		}
	}
	return nil
}

func cloneScope(scope map[string]any) map[string]any {
	out := make(map[string]any, len(scope)+4)
	for k, v := range scope {
		out[k] = v
	}
	return out
}

// stubScope builds the sample data for a state's scope. The JS side wraps it
// in throwing proxies.
func (m *Machine) stubScope(s *State) map[string]any {
	ctx := map[string]any{}
	if len(m.Input) == 0 {
		// No declared inputs: run input keys are unknowable, so ctx cannot
		// be checked strictly. Declaring input: buys strict checking.
		ctx[openMarkerKey] = true
	}
	for name, spec := range m.Input {
		ctx[name] = sampleForType(spec.Type)
	}
	for _, p := range m.States {
		if p.Terminal || p.Name == s.Name {
			continue
		}
		if reachableFrom(m, p.Name)[s.Name] {
			ctx[p.Name] = m.outputStub(p)
		}
	}
	// A state's own output is visible to itself on revisits (loops).
	ctx[s.Name] = m.outputStub(s)

	visits := map[string]any{}
	for _, st := range m.States {
		visits[st.Name] = 0
	}
	return map[string]any{
		"ctx":     ctx,
		"visits":  visits,
		"run":     map[string]any{"transitions": 0, "tokens": 0, "cost": 0.0},
		"attempt": 1,
		"output":  anyMarker(),
		"event":   "",
	}
}

// outputStub models what ctx.<state> looks like downstream.
func (m *Machine) outputStub(s *State) any {
	var body any = anyMarker() // actions and schema-less agents are opaque
	if s.Agent != nil && len(s.Output.Schema) > 0 {
		if normalized, err := NormalizeSchema(s.Output.Schema); err == nil {
			shape := map[string]any{}
			for k, frag := range normalized {
				shape[k] = sampleFromSchema(frag)
			}
			if len(s.Output.Events) > 0 {
				shape["event"] = s.Output.Events[0]
			}
			body = shape
		}
	}
	if s.ForEach != nil {
		return map[string]any{
			"items":     []any{body},
			"count":     1,
			"skipped":   0,
			"failures":  []any{},
			"memo_hits": 0,
		}
	}
	return body
}

func sampleFromSchema(frag any) any {
	f, ok := frag.(map[string]any)
	if !ok {
		return anyMarker()
	}
	if enum, ok := f["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	switch f["type"] {
	case "string":
		return "sample"
	case "number", "integer":
		return 1
	case "boolean":
		return true
	case "array":
		return []any{sampleFromSchema(f["items"])}
	case "object":
		props, _ := f["properties"].(map[string]any)
		out := map[string]any{}
		for k, p := range props {
			out[k] = sampleFromSchema(p)
		}
		if len(out) == 0 {
			return anyMarker()
		}
		return out
	}
	return anyMarker()
}

func sampleForType(t string) any {
	switch t {
	case "number", "integer":
		return 1
	case "boolean":
		return true
	case "object", "array", "":
		return anyMarker()
	}
	return "sample"
}

// ScopeDoc renders what a state's functions may reference — the
// discoverability answer, derived from the same shapes the dry-run uses.
func ScopeDoc(m *Machine, s *State) string {
	var b strings.Builder
	scope := m.stubScope(s)

	fmt.Fprintf(&b, "scope for functions in state %q:\n", s.Name)
	ctx, _ := scope["ctx"].(map[string]any)
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  ctx.%s%s\n", k, describeShape(ctx[k], "    "))
	}
	if f := s.ForEach; f != nil {
		fmt.Fprintf(&b, "  %s — the current item (shape depends on over)\n", f.As)
		fmt.Fprintf(&b, "  index, total — item position\n")
	}
	fmt.Fprintf(&b, "  visits.<state> — entry counts; run.{transitions, tokens, cost}; attempt\n")
	if len(s.Transitions) > 0 {
		fmt.Fprintf(&b, "  output, event — this state's result (transition guards only)\n")
	}
	if s.Agent != nil && len(s.Agent.Tools) > 0 {
		fmt.Fprintf(&b, "  args, calls.<tool>, turn — tool guards only\n")
	}
	return b.String()
}

func describeShape(v any, indent string) string {
	switch t := v.(type) {
	case map[string]any:
		if t[anyMarkerKey] == true {
			return " (opaque — no declared schema)"
		}
		var b strings.Builder
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("\n" + indent + "." + k + describeShape(t[k], indent+"  "))
		}
		return b.String()
	case []any:
		if len(t) == 1 {
			return "[]" + describeShape(t[0], indent)
		}
		return " []"
	case string:
		return ": string"
	case int, int64, float64:
		return ": number"
	case bool:
		return ": boolean"
	}
	return ""
}

const anyMarkerKey = "__steps_any__"
const openMarkerKey = "__steps_open__"

// anyMarker marks a region of unknown shape; the JS wrapper turns it into a
// permissive stub that tolerates any access, call, or iteration.
func anyMarker() map[string]any { return map[string]any{anyMarkerKey: true} }

// installStubs registers the proxy helpers in the machine's runtime.
func (rt *jsRT) installStubs() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.vm.Get("__stepsStub") != nil {
		return nil
	}
	_, err := rt.vm.RunString(stubHelperJS)
	return err
}

// stubScope wraps sample data into throwing/permissive proxies.
func (rt *jsRT) stubScope(scope map[string]any) (map[string]any, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	wrap, ok := goja.AssertFunction(rt.vm.Get("__stepsStub"))
	if !ok {
		return nil, fmt.Errorf("stub helper not installed")
	}
	out := make(map[string]any, len(scope))
	for k, v := range scope {
		wrapped, err := wrap(goja.Undefined(), rt.vm.ToValue(v), rt.vm.ToValue(k))
		if err != nil {
			return nil, err
		}
		out[k] = wrapped
	}
	return out, nil
}

const stubHelperJS = `
(function () {
  const ANY = "` + anyMarkerKey + `";
  const OPEN = "` + openMarkerKey + `";
  function anyStub() {
    const f = function () { return anyStub(); };
    return new Proxy(f, {
      get(t, k) {
        if (k === Symbol.toPrimitive || k === "toString" || k === "valueOf") return () => "«stub»";
        if (k === Symbol.iterator) return function* () { yield anyStub(); };
        if (k === "length") return 1;
        return anyStub();
      },
      apply() { return anyStub(); },
      has() { return true; },
    });
  }
  function wrap(v, path) {
    if (v === null || v === undefined) return v;
    if (Array.isArray(v)) return v.map((e, i) => wrap(e, path + "[" + i + "]"));
    if (typeof v === "object") {
      if (v[ANY]) return anyStub();
      const open = !!v[OPEN];
      const out = {};
      for (const k of Object.keys(v)) {
        if (k === OPEN) continue;
        out[k] = wrap(v[k], path + "." + k);
      }
      return new Proxy(out, {
        get(t, k) {
          if (typeof k === "symbol") return t[k];
          if (k in t) return t[k];
          if (open) return anyStub();
          throw new Error("unknown field " + path + "." + String(k) + " — available: " + Object.keys(t).join(", "));
        },
      });
    }
    return v;
  }
  globalThis.__stepsStub = wrap;
})();
`
