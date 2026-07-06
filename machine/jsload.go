package machine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
	esbuild "github.com/evanw/esbuild/pkg/api"
	"gopkg.in/yaml.v3"
)

// ParseOption adjusts a machine between evaluation and defaults expansion —
// the hook for engine-level defaults (the last rung of the cascade).
type ParseOption func(*Machine)

// WithEngineDefaultModel supplies the engine-level default model, used only
// when neither the state nor the machine's defaults name one.
func WithEngineDefaultModel(model string) ParseOption {
	return func(m *Machine) {
		if m.Defaults.Agent.Model == "" {
			m.Defaults.Agent.Model = model
		}
	}
}

// Load reads, transpiles (TypeScript), evaluates, expands, compiles, and
// validates a machine from a .ts or .js file. include() paths resolve
// relative to the file.
func Load(path string, opts ...ParseOption) (*Machine, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading machine file %s: %w", path, err)
	}
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		return nil, fmt.Errorf("%s: machines are TypeScript (export default {...}); YAML machine files are no longer supported", path)
	}
	l := &loader{dir: filepath.Dir(path), assets: map[string]string{}, sourcefile: filepath.Base(path)}
	return l.parse(src, opts...)
}

// Parse builds a machine from JS source (include() relative to cwd).
func Parse(src []byte, opts ...ParseOption) (*Machine, error) {
	return parseSource(src, ".", opts...)
}

// ParseWithAssets rebuilds a machine from pinned source + assets — the
// resume path. include() reads from the pinned assets, never the filesystem.
func ParseWithAssets(src []byte, assets map[string]string, opts ...ParseOption) (*Machine, error) {
	l := &loader{dir: "", assets: assets, pinned: true}
	return l.parse(src, opts...)
}

func parseSource(src []byte, dir string, opts ...ParseOption) (*Machine, error) {
	l := &loader{dir: dir, assets: map[string]string{}}
	return l.parse(src, opts...)
}

type loader struct {
	rt         *jsRT
	dir        string
	assets     map[string]string
	pinned     bool   // resume: include() serves pinned assets only
	sourcefile string // for esbuild error locations (defaults to machine.ts)
	// parallelNodes maps a fork state's name to its raw parallel: object, so
	// compileFlow can wire the branch sub-flows after every state is registered.
	parallelNodes map[string]*goja.Object
}

// transpile strips TypeScript and lowers `export default` to CommonJS so goja
// (no ESM, no types) can run it. TS is a superset of JS, so plain-JS machines
// pass through unchanged. `/// <reference>` directives are editor-only and
// carried through as comments — esbuild does not resolve them in transform mode.
func (l *loader) transpile(src []byte) (string, error) {
	name := l.sourcefile
	if name == "" {
		name = "machine.ts"
	}
	result := esbuild.Transform(string(src), esbuild.TransformOptions{
		Loader:     esbuild.LoaderTS,
		Format:     esbuild.FormatCommonJS, // export default -> module.exports.default
		Target:     esbuild.ES2020,         // goja-safe; keeps spread/optional-chaining
		Sourcefile: name,
	})
	if len(result.Errors) > 0 {
		var msgs []string
		for _, e := range result.Errors {
			if e.Location != nil {
				msgs = append(msgs, fmt.Sprintf("%s:%d: %s", e.Location.File, e.Location.Line, e.Text))
			} else {
				msgs = append(msgs, e.Text)
			}
		}
		return "", fmt.Errorf("transpiling machine: %s", strings.Join(msgs, "; "))
	}
	return string(result.Code), nil
}

func (l *loader) parse(src []byte, opts ...ParseOption) (*Machine, error) {
	vm := goja.New()
	l.rt = &jsRT{vm: vm}

	module := vm.NewObject()
	_ = module.Set("exports", vm.NewObject())
	_ = vm.Set("module", module)
	_ = vm.Set("include", l.include)
	_ = vm.Set("yaml", func(v any) (string, error) {
		raw, err := yaml.Marshal(v)
		return strings.TrimRight(string(raw), "\n"), err
	})
	_, err := vm.RunString(flowBootstrapJS)
	if err != nil {
		return nil, fmt.Errorf("installing flow combinators: %w", err)
	}

	code, err := l.transpile(src)
	if err != nil {
		return nil, err
	}
	_, err = vm.RunString(code)
	if err != nil {
		return nil, fmt.Errorf("evaluating machine: %w", err)
	}
	exports, ok := module.Get("exports").(*goja.Object)
	if !ok {
		return nil, errors.New("machine must export default { name, states, ... }")
	}
	// esbuild's CommonJS output puts the default export under .default
	// (alongside a synthetic __esModule flag); unwrap to the machine object.
	root := exports
	if def := exports.Get("default"); defined(def) {
		if o := l.obj(def); o != nil {
			root = o
		}
	}
	if len(root.Keys()) == 0 {
		return nil, errors.New("machine must export default { name, states, ... }")
	}

	m, err := l.machine(root)
	if err != nil {
		return nil, err
	}
	m.Source = src
	m.Assets = l.assets
	m.Hash = hashMachine(src, l.assets)
	m.rt = l.rt

	for _, o := range opts {
		o(m)
	}
	ApplyDefaults(m)
	err = Compile(m)
	if err != nil {
		return nil, err
	}
	err = Validate(m)
	if err != nil {
		return nil, err
	}
	// Fail before you spend: destructured parameters are each function's
	// declared contract; then every function is exercised against
	// schema-derived stubs. Impossible access fails the load.
	err = CheckContracts(m)
	if err != nil {
		return nil, err
	}
	if fatals, _ := DryRun(m); len(fatals) > 0 {
		return nil, errors.Join(fatals...)
	}
	return m, nil
}

// include loads a text asset (prompt files). Contents are pinned with the
// machine so resume never depends on the filesystem.
func (l *loader) include(path string) (string, error) {
	if content, ok := l.assets[path]; ok {
		return content, nil
	}
	if l.pinned {
		return "", fmt.Errorf("include(%q): not in pinned assets", path)
	}
	joined := filepath.Join(l.dir, path)
	rel, err := filepath.Rel(l.dir, joined)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("include(%q): escapes the machine's directory", path)
	}
	raw, err := os.ReadFile(joined)
	if err != nil {
		return "", fmt.Errorf("include(%q): %w", path, err)
	}
	l.assets[path] = string(raw)
	return string(raw), nil
}

// ---- exported-object walking -----------------------------------------------

func defined(v goja.Value) bool {
	return v != nil && !goja.IsUndefined(v) && !goja.IsNull(v)
}

func (l *loader) obj(v goja.Value) *goja.Object {
	if !defined(v) {
		return nil
	}
	if o, ok := v.(*goja.Object); ok {
		return o
	}
	return nil
}

// exportValue converts a JS value to Go data; functions become Dyn.
func (l *loader) exportValue(v goja.Value) any {
	if !defined(v) {
		return nil
	}
	if fn, ok := goja.AssertFunction(v); ok {
		return Dyn{fn: fn, Src: v.String(), rt: l.rt}
	}
	if o, ok := v.(*goja.Object); ok {
		if o.ClassName() == "Array" {
			n := int(o.Get("length").ToInteger())
			out := make([]any, 0, n)
			for i := range n {
				out = append(out, l.exportValue(o.Get(strconv.Itoa(i))))
			}
			return out
		}
		out := make(map[string]any, len(o.Keys()))
		for _, k := range o.Keys() {
			out[k] = l.exportValue(o.Get(k))
		}
		return out
	}
	return v.Export()
}

// exportData is exportValue for pure-data positions (schemas): functions are
// an error.
func (l *loader) exportData(v goja.Value, where string) (any, error) {
	out := l.exportValue(v)
	err := noFns(out, where)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func noFns(v any, where string) error {
	switch t := v.(type) {
	case Dyn:
		return fmt.Errorf("%s must be data, not a function", where)
	case map[string]any:
		for k, e := range t {
			err := noFns(e, where+"."+k)
			if err != nil {
				return err
			}
		}
	case []any:
		for i, e := range t {
			err := noFns(e, fmt.Sprintf("%s[%d]", where, i))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *loader) dyn(v goja.Value) Dyn {
	if !defined(v) {
		return Dyn{}
	}
	if fn, ok := goja.AssertFunction(v); ok {
		return Dyn{fn: fn, Src: v.String(), rt: l.rt}
	}
	return Dyn{Static: l.exportValue(v)}
}

func str(v goja.Value) string {
	if !defined(v) {
		return ""
	}
	return v.String()
}

func integer(v goja.Value) int {
	if !defined(v) {
		return 0
	}
	return int(v.ToInteger())
}

func boolean(v goja.Value) bool {
	return defined(v) && v.ToBoolean()
}

func duration(v goja.Value, where string) (time.Duration, error) {
	if !defined(v) {
		return 0, nil
	}
	d, err := time.ParseDuration(v.String())
	if err != nil {
		return 0, fmt.Errorf("%s: %w (use Go duration strings like \"30m\")", where, err)
	}
	return d, nil
}

func stringSlice(v goja.Value) []string {
	o, ok := v.(*goja.Object)
	if !defined(v) || !ok {
		return nil
	}
	n := int(o.Get("length").ToInteger())
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, o.Get(strconv.Itoa(i)).String())
	}
	return out
}

// choices parses a gate's choices: declaration. Two forms, discriminated by
// the reserved `multi` key: {event: label, ...} (single/confirm — each key is
// one of the gate's resume events) or {multi: [...]|fn, event?, min?, max?}
// (multi-select — one event, selection in output).
func (l *loader) choices(v goja.Value) (*ChoiceSpec, error) {
	if !defined(v) {
		return nil, nil //nolint:nilnil // no choices: declared is a valid absence, not an error
	}
	o := l.obj(v)
	if o == nil {
		return nil, errors.New("choices must be an object")
	}
	keys := o.Keys()
	if !contains(keys, "multi") {
		// Single/confirm: {resumeEvent: label}, declaration order preserved.
		spec := &ChoiceSpec{Kind: "single"}
		for _, k := range keys {
			label, ok := o.Get(k).Export().(string)
			if !ok {
				return nil, fmt.Errorf("choices.%s: label must be a string", k)
			}
			spec.Options = append(spec.Options, ChoiceOption{Event: k, Label: label})
		}
		if len(spec.Options) == 0 {
			return nil, errors.New("choices must declare at least one option")
		}
		return spec, nil
	}
	for _, k := range keys {
		if !contains([]string{"multi", "event", "min", "max"}, k) {
			return nil, fmt.Errorf("unknown key %q for multi choices — valid keys: multi, event, min, max", k)
		}
	}
	spec := &ChoiceSpec{
		Kind:    "multi",
		Dynamic: l.dyn(o.Get("multi")),
		Event:   str(o.Get("event")),
		Min:     integer(o.Get("min")),
		Max:     integer(o.Get("max")),
	}
	if !spec.Dynamic.IsFn() {
		items, ok := spec.Dynamic.Static.([]any)
		if !ok {
			return nil, errors.New("choices.multi must be an array of strings or a function of scope")
		}
		for i, it := range items {
			if _, ok := it.(string); !ok {
				return nil, fmt.Errorf("choices.multi[%d] must be a string", i)
			}
		}
	}
	return spec, nil
}

// ---- machine construction ---------------------------------------------------

// Top-level machine keys. Anything else is a load error — flat formats need
// hard typo protection.
var machineKeys = []string{
	"name", "version", "description", "input", "models", "model",
	"defaults", "limits", "initial", "states", "flow", "webhook",
}

// applyWebhook parses a trigger-only webhook: block onto m, if present. Path
// defaults to the machine name; map is a function of scope returning inputs.
func (l *loader) applyWebhook(m *Machine, root *goja.Object) error {
	o := l.obj(root.Get("webhook"))
	if o == nil {
		return nil
	}
	w := &WebhookSpec{}
	for _, k := range o.Keys() {
		switch k {
		case "path":
			w.Path = str(o.Get(k))
		case "map":
			w.Map = l.dyn(o.Get(k))
		case "maxInFlight":
			w.MaxInFlight = integer(o.Get(k))
		case "maxQueued":
			w.MaxQueued = integer(o.Get(k))
		default:
			return fmt.Errorf("unknown webhook key %q — valid: path, map, maxInFlight, maxQueued", k)
		}
	}
	if w.Path == "" {
		w.Path = m.Name
	}
	m.Webhook = w
	return nil
}

func (l *loader) machine(root *goja.Object) (*Machine, error) {
	for _, k := range root.Keys() {
		if !contains(machineKeys, k) {
			return nil, fmt.Errorf("unknown machine key %q — valid keys: %s", k, strings.Join(machineKeys, ", "))
		}
	}

	m := &Machine{
		Version:     integer(root.Get("version")),
		Name:        str(root.Get("name")),
		Description: str(root.Get("description")),
		Initial:     str(root.Get("initial")),
	}

	if o := l.obj(root.Get("input")); o != nil {
		m.Input = l.parseInputSpecs(o)
	}

	if o := l.obj(root.Get("models")); o != nil {
		m.Models = map[string]string{}
		for _, k := range o.Keys() {
			m.Models[k] = str(o.Get(k))
		}
	}

	// model: top-level sugar for the default agent model.
	if v := root.Get("model"); defined(v) {
		m.Defaults.Agent.Model = v.String()
	}

	// defaults: FLAT — agent knobs and retry policies directly.
	if o := l.obj(root.Get("defaults")); o != nil {
		err := l.applyMachineDefaults(m, o)
		if err != nil {
			return nil, err
		}
	}

	if o := l.obj(root.Get("limits")); o != nil {
		err := applyMachineLimits(m, o)
		if err != nil {
			return nil, err
		}
	}

	err := l.applyWebhook(m, root)
	if err != nil {
		return nil, err
	}

	err = l.parseStates(m, root)
	if err != nil {
		return nil, err
	}
	m.buildIndex()

	if flow := root.Get("flow"); defined(flow) {
		err := l.compileFlow(m, flow)
		if err != nil {
			return nil, err
		}
	} else if len(l.parallelNodes) > 0 {
		// Branch sub-flows are wired only inside compileFlow; a fork needs an
		// explicit flow to place itself and its join.
		return nil, errors.New("a machine with a parallel: state needs a flow: expression to place the fork and its join")
	}
	return m, nil
}

// parseInputSpecs parses the machine's input: block, supporting the
// shorthand form (article: "string") alongside the full {type, required} form.
func (l *loader) parseInputSpecs(o *goja.Object) map[string]InputSpec {
	input := map[string]InputSpec{}
	for _, k := range o.Keys() {
		v := o.Get(k)
		if s, ok := v.Export().(string); ok {
			input[k] = InputSpec{Type: s}
			continue
		}
		spec := l.obj(v)
		is := InputSpec{}
		if spec != nil {
			is.Type = str(spec.Get("type"))
			is.Required = boolean(spec.Get("required"))
		}
		input[k] = is
	}
	return input
}

// applyMachineDefaults parses the machine's flat defaults: block (agent
// knobs and retry policies directly, not nested under an "agent" key).
func (l *loader) applyMachineDefaults(m *Machine, o *goja.Object) error {
	for _, k := range o.Keys() {
		switch k {
		case "model":
			m.Defaults.Agent.Model = str(o.Get(k))
		case "maxTurns":
			m.Defaults.Agent.MaxTurns = integer(o.Get(k))
		case "maxOutputTokens":
			m.Defaults.Agent.MaxOutputTokens = integer(o.Get(k))
		case "maxInputTokens":
			maxInput := integer(o.Get(k))
			m.Defaults.Agent.MaxInputTokens = &maxInput
		case "temperature":
			t := o.Get(k).ToFloat()
			m.Defaults.Agent.Temperature = &t
		case "reasoning":
			m.Defaults.Agent.Reasoning = str(o.Get(k))
		case "structuredOutput":
			m.Defaults.Agent.StructuredOutput = str(o.Get(k))
		case "retry":
			r, err := l.retries(o.Get(k), "defaults.retry")
			if err != nil {
				return err
			}
			m.Defaults.Retry = r
		default:
			return fmt.Errorf("unknown defaults key %q — valid: model, maxTurns, maxOutputTokens, maxInputTokens, temperature, reasoning, structuredOutput, retry", k)
		}
	}
	return nil
}

func applyMachineLimits(m *Machine, o *goja.Object) error {
	for _, k := range o.Keys() {
		switch k {
		case "maxTransitions":
			m.Limits.MaxTransitions = integer(o.Get(k))
		case "maxCost":
			m.Limits.MaxCost = o.Get(k).ToFloat()
		case "maxTokens":
			m.Limits.MaxTokens = integer(o.Get(k))
		case "timeout":
			d, err := duration(o.Get(k), "limits.timeout")
			if err != nil {
				return err
			}
			m.Limits.Timeout = d
		default:
			return fmt.Errorf("unknown limits key %q — valid: maxTransitions, maxCost, maxTokens, timeout", k)
		}
	}
	return nil
}

// parseStates parses the machine's required states: block in declaration
// order (linear-flow defaults depend on it), marking each state object's
// identity so the flow expression can reference the const.
func (l *loader) parseStates(m *Machine, root *goja.Object) error {
	states := l.obj(root.Get("states"))
	if states == nil || len(states.Keys()) == 0 {
		return errors.New("machine has no states — export default { states: { ... } }")
	}
	for _, name := range states.Keys() {
		if containsState(m.States, name) {
			return fmt.Errorf("state %q declared twice", name)
		}
		v := states.Get(name)
		st, err := l.state(name, v)
		if err != nil {
			return fmt.Errorf("state %q: %w", name, err)
		}
		m.States = append(m.States, st)
		if obj := l.obj(v); obj != nil {
			_ = obj.DefineDataProperty(stateNameProp, l.rt.vm.ToValue(name), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func containsState(states []*State, name string) bool {
	for _, s := range states {
		if s.Name == name {
			return true
		}
	}
	return false
}

// State keys, by handler. Shared keys apply to every handler.
var (
	sharedStateKeys = []string{"memo", "forEach", "distill", "retry", "output", "events", "input"}
	agentStateKeys  = []string{"prompt", "system", "tools", "model", "maxTurns",
		"maxOutputTokens", "maxInputTokens", "temperature", "reasoning",
		"structuredOutput", "toolChoice", "adopt", "history"}
	actionStateKeys   = []string{"action"}
	writeStateKeys    = []string{"write", "content"}
	humanStateKeys    = []string{"human", "timeout", "choices"}
	parallelStateKeys = []string{"parallel", "concurrency", "onBranchFailure"}
	terminalStateKeys = []string{"terminal", "status"}
	movedToFlowKeys   = []string{"transitions", "catch", "onTimeout", "to", "next"}
)

func (l *loader) state(name string, v goja.Value) (*State, error) {
	// Bare-string state: the whole state is an agent prompt.
	if s, ok := v.Export().(string); ok {
		return &State{Name: name, Agent: &AgentSpec{Prompt: Dyn{Static: s}}}, nil
	}
	if fn, ok := goja.AssertFunction(v); ok {
		return &State{Name: name, Agent: &AgentSpec{Prompt: Dyn{fn: fn, Src: v.String(), rt: l.rt}}}, nil
	}
	o := l.obj(v)
	if o == nil {
		return nil, errors.New("a state must be an object, a prompt string, or a prompt function")
	}

	handler, handlerKeys := inferStateHandler(o.Keys())
	err := checkStateKeys(o.Keys(), handler, handlerKeys)
	if err != nil {
		return nil, err
	}

	st := &State{
		Name:  name,
		Memo:  boolean(o.Get("memo")),
		Input: l.dyn(o.Get("input")),
	}
	err = l.buildStateHandler(o, handler, st)
	if err != nil {
		return nil, err
	}

	if f := l.obj(o.Get("forEach")); f != nil {
		st.ForEach = &ForEachSpec{
			Over:          l.dyn(f.Get("over")),
			As:            str(f.Get("as")),
			Concurrency:   integer(f.Get("concurrency")),
			OnItemFailure: str(f.Get("onItemFailure")),
		}
	}

	distill, err := l.parseStateDistill(o)
	if err != nil {
		return nil, err
	}
	st.Distill = distill

	err = l.applyStateOutput(o, st)
	if err != nil {
		return nil, err
	}

	r, err := l.retries(o.Get("retry"), "retry")
	if err != nil {
		return nil, err
	}
	if r != nil {
		st.Retry = r
	}

	return st, nil
}

// inferStateHandler picks the state's handler from the keys present, and
// the key set valid for that handler.
func inferStateHandler(keys []string) (handler string, handlerKeys []string) {
	has := func(k string) bool { return contains(keys, k) }
	switch {
	case has("action"):
		return "action", actionStateKeys
	case has("write"):
		return "write", writeStateKeys
	case has("human"):
		return "human", humanStateKeys
	case has("parallel"):
		return "parallel", parallelStateKeys
	case has("terminal"):
		return "terminal", terminalStateKeys
	default:
		return "agent", agentStateKeys
	}
}

func checkStateKeys(keys []string, handler string, handlerKeys []string) error {
	valid := append(append([]string{}, sharedStateKeys...), handlerKeys...)
	for _, k := range keys {
		if k == stateNameProp {
			continue
		}
		if contains(movedToFlowKeys, k) {
			return fmt.Errorf("key %q moved to the flow expression — wire routing with pipe/branch/when", k)
		}
		if !contains(valid, k) {
			return fmt.Errorf("unknown key %q for a %s state — valid keys: %s", k, handler, strings.Join(valid, ", "))
		}
	}
	return nil
}

// buildStateHandler fills in the state's handler-specific fields.
func (l *loader) buildStateHandler(o *goja.Object, handler string, st *State) error {
	switch handler {
	case "terminal":
		st.Terminal = true
		st.Status = str(o.Get("status"))
	case "action":
		st.Action = &ActionSpec{Name: str(o.Get("action"))}
	case "write":
		st.Action = &ActionSpec{Name: "file.write"}
		if !st.Input.IsZero() {
			return errors.New("write states take write: or content:, not input")
		}
		st.Input = Dyn{Static: map[string]any{
			"path":    l.exportValue(o.Get("write")),
			"content": l.exportValue(o.Get("content")),
		}}
	case "human":
		timeout, err := duration(o.Get("timeout"), "timeout")
		if err != nil {
			return err
		}
		choices, err := l.choices(o.Get("choices"))
		if err != nil {
			return err
		}
		st.Human = &HumanSpec{Prompt: l.dyn(o.Get("human")), Timeout: timeout, Choices: choices}
	case "parallel":
		pobj := l.obj(o.Get("parallel"))
		if pobj == nil {
			return errors.New("parallel: must be an object of {label: state | pipe(...) | branch(...) | loop(...)}")
		}
		if l.parallelNodes == nil {
			l.parallelNodes = map[string]*goja.Object{}
		}
		// Branch sub-flows are wired at flow-compile time (compileFlow), once
		// every state object carries its identity — mirror loop/branch.
		l.parallelNodes[st.Name] = pobj
		st.Parallel = &ParallelSpec{
			Concurrency:     integer(o.Get("concurrency")),
			OnBranchFailure: str(o.Get("onBranchFailure")),
		}
	case "agent":
		ag, err := l.agent(o)
		if err != nil {
			return err
		}
		st.Agent = ag
	}
	return nil
}

// parseStateDistill parses the state's distill: {name: {for, from?,
// maxTokens?, model?, memo?}} block, preserving declaration order (the
// implicit chain runs in it).
func (l *loader) parseStateDistill(o *goja.Object) ([]DistillEntry, error) {
	v := o.Get("distill")
	if !defined(v) {
		return nil, nil
	}
	d := l.obj(v)
	if d == nil {
		return nil, errors.New("distill must be an object of {name: {for, from?, maxTokens?, model?, memo?}}")
	}
	var entries []DistillEntry
	for _, key := range d.Keys() {
		eo := l.obj(d.Get(key))
		if eo == nil {
			return nil, fmt.Errorf("distill.%s must be an object {for, from?, maxTokens?, model?, memo?}", key)
		}
		for _, k := range eo.Keys() {
			if !contains([]string{"for", "from", "maxTokens", "model", "memo"}, k) {
				return nil, fmt.Errorf("distill.%s: unknown key %q — valid keys: for, from, maxTokens, model, memo", key, k)
			}
		}
		entry := DistillEntry{
			Key:       key,
			From:      str(eo.Get("from")),
			For:       l.dyn(eo.Get("for")),
			MaxTokens: integer(eo.Get("maxTokens")),
			Model:     str(eo.Get("model")),
			Memo:      true, // distillation is pure; replay is always safe
		}
		if defined(eo.Get("memo")) {
			entry.Memo = boolean(eo.Get("memo"))
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// applyStateOutput parses the state's output: schema and events:.
func (l *loader) applyStateOutput(o *goja.Object, st *State) error {
	if out := o.Get("output"); defined(out) {
		schema, err := l.exportData(out, "output")
		if err != nil {
			return err
		}
		sm, ok := schema.(map[string]any)
		if !ok {
			return errors.New("output must be a schema object")
		}
		st.Output.Schema = sm
	}
	st.Output.Events = stringSlice(o.Get("events"))
	return nil
}

func (l *loader) agent(o *goja.Object) (*AgentSpec, error) {
	ag := &AgentSpec{
		Model:            l.dyn(o.Get("model")),
		System:           l.dyn(o.Get("system")),
		Prompt:           l.dyn(o.Get("prompt")),
		MaxTurns:         integer(o.Get("maxTurns")),
		MaxOutputTokens:  integer(o.Get("maxOutputTokens")),
		StructuredOutput: str(o.Get("structuredOutput")),
		Reasoning:        str(o.Get("reasoning")),
		ToolChoice:       str(o.Get("toolChoice")),
	}
	if v := o.Get("maxInputTokens"); defined(v) {
		maxInput := int(v.ToInteger())
		ag.MaxInputTokens = &maxInput
	}
	if defined(o.Get("temperature")) {
		t := o.Get("temperature").ToFloat()
		ag.Temperature = &t
	}

	switch adopt := o.Get("adopt"); {
	case !defined(adopt):
	case l.obj(adopt) != nil:
		a := l.obj(adopt)
		ag.Adopt = str(a.Get("from"))
		ag.AdoptLastTurns = integer(a.Get("lastTurns"))
		if ag.Adopt == "" {
			return nil, errors.New("adopt: object form requires from")
		}
	default:
		ag.Adopt = adopt.String()
	}

	if h := l.obj(o.Get("history")); h != nil {
		ag.History = &HistorySpec{
			From:      str(h.Get("from")),
			Include:   stringSlice(h.Get("include")),
			LastTurns: integer(h.Get("lastTurns")),
			As:        str(h.Get("as")),
		}
	}

	if tools := l.obj(o.Get("tools")); tools != nil {
		n := int(tools.Get("length").ToInteger())
		for i := range n {
			tv := tools.Get(strconv.Itoa(i))
			if s, ok := tv.Export().(string); ok {
				ag.Tools = append(ag.Tools, ToolRef{Name: s})
				continue
			}
			to := l.obj(tv)
			if to == nil {
				return nil, fmt.Errorf("tools[%d] must be a name or an object", i)
			}
			ag.Tools = append(ag.Tools, ToolRef{
				Name:     str(to.Get("name")),
				MaxCalls: integer(to.Get("maxCalls")),
				When:     l.dyn(to.Get("when")),
				OnReject: str(to.Get("onReject")),
				Require:  str(to.Get("require")),
				Args:     l.dyn(to.Get("args")),
			})
		}
	}
	return ag, nil
}

func (l *loader) retries(v goja.Value, where string) ([]RetryPolicy, error) {
	if !defined(v) {
		return nil, nil
	}
	if s, ok := v.Export().(string); ok && s == "none" {
		return []RetryPolicy{}, nil
	}
	o := l.obj(v)
	if o == nil {
		return nil, fmt.Errorf("%s must be an array or \"none\"", where)
	}
	n := int(o.Get("length").ToInteger())
	out := make([]RetryPolicy, 0, n)
	for i := range n {
		e := l.obj(o.Get(strconv.Itoa(i)))
		if e == nil {
			return nil, fmt.Errorf("%s[%d] must be an object", where, i)
		}
		rp := RetryPolicy{
			Match:       stringSlice(e.Get("match")),
			MaxAttempts: integer(e.Get("maxAttempts")),
		}
		if b := l.obj(e.Get("backoff")); b != nil {
			initial, err := duration(b.Get("initial"), where+".backoff.initial")
			if err != nil {
				return nil, err
			}
			backoffCap, err := duration(b.Get("cap"), where+".backoff.cap")
			if err != nil {
				return nil, err
			}
			rp.Backoff = Backoff{
				Initial: initial,
				Factor:  b.Get("factor").ToFloat(),
				Jitter:  boolean(b.Get("jitter")),
				Cap:     backoffCap,
			}
		}
		out = append(out, rp)
	}
	return out, nil
}
