package machine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dop251/goja"
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

// Load reads, evaluates, expands, compiles, and validates a machine from a
// .js file. include() paths resolve relative to the file.
func Load(path string, opts ...ParseOption) (*Machine, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		return nil, fmt.Errorf("%s: machines are JavaScript now (module.exports = {...}); YAML machine files are no longer supported", path)
	}
	return parseSource(src, filepath.Dir(path), opts...)
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
	rt     *jsRT
	dir    string
	assets map[string]string
	pinned bool // resume: include() serves pinned assets only
}

func (l *loader) parse(src []byte, opts ...ParseOption) (*Machine, error) {
	vm := goja.New()
	l.rt = &jsRT{vm: vm}

	module := vm.NewObject()
	_ = module.Set("exports", vm.NewObject())
	_ = vm.Set("module", module)
	_ = vm.Set("include", l.include)

	if _, err := vm.RunString(string(src)); err != nil {
		return nil, fmt.Errorf("evaluating machine: %w", err)
	}
	exports, ok := module.Get("exports").(*goja.Object)
	if !ok || len(exports.Keys()) == 0 {
		return nil, fmt.Errorf("machine must assign module.exports = { name, states, ... }")
	}

	m, err := l.machine(exports)
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
	if err := Compile(m); err != nil {
		return nil, err
	}
	if err := Validate(m); err != nil {
		return nil, err
	}
	// Fail before you spend: every function is exercised against
	// schema-derived stubs; impossible field access fails the load.
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
			for i := 0; i < n; i++ {
				out = append(out, l.exportValue(o.Get(fmt.Sprintf("%d", i))))
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
	if err := noFns(out, where); err != nil {
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
			if err := noFns(e, where+"."+k); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range t {
			if err := noFns(e, fmt.Sprintf("%s[%d]", where, i)); err != nil {
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
	for i := 0; i < n; i++ {
		out = append(out, o.Get(fmt.Sprintf("%d", i)).String())
	}
	return out
}

// ---- machine construction ---------------------------------------------------

func (l *loader) machine(root *goja.Object) (*Machine, error) {
	m := &Machine{
		Version:     integer(root.Get("version")),
		Name:        str(root.Get("name")),
		Description: str(root.Get("description")),
		Initial:     str(root.Get("initial")),
	}

	if o := l.obj(root.Get("input")); o != nil {
		m.Input = map[string]InputSpec{}
		for _, k := range o.Keys() {
			spec := l.obj(o.Get(k))
			is := InputSpec{}
			if spec != nil {
				is.Type = str(spec.Get("type"))
				is.Required = boolean(spec.Get("required"))
			}
			m.Input[k] = is
		}
	}

	if o := l.obj(root.Get("models")); o != nil {
		m.Models = map[string]string{}
		for _, k := range o.Keys() {
			m.Models[k] = str(o.Get(k))
		}
	}

	if o := l.obj(root.Get("defaults")); o != nil {
		if a := l.obj(o.Get("agent")); a != nil {
			m.Defaults.Agent = AgentDefaults{
				Model:            str(a.Get("model")),
				MaxTurns:         integer(a.Get("maxTurns")),
				MaxOutputTokens:  integer(a.Get("maxOutputTokens")),
				StructuredOutput: str(a.Get("structuredOutput")),
				Reasoning:        str(a.Get("reasoning")),
			}
			if defined(a.Get("temperature")) {
				t := a.Get("temperature").ToFloat()
				m.Defaults.Agent.Temperature = &t
			}
		}
		if r, err := l.retries(o.Get("retry"), "defaults.retry"); err != nil {
			return nil, err
		} else if r != nil {
			m.Defaults.Retry = r
		}
	}

	if o := l.obj(root.Get("limits")); o != nil {
		m.Limits.MaxTransitions = integer(o.Get("maxTransitions"))
		if defined(o.Get("maxCost")) {
			m.Limits.MaxCost = o.Get("maxCost").ToFloat()
		}
		m.Limits.MaxTokens = integer(o.Get("maxTokens"))
		d, err := duration(o.Get("timeout"), "limits.timeout")
		if err != nil {
			return nil, err
		}
		m.Limits.Timeout = d
	}

	states := l.obj(root.Get("states"))
	if states == nil || len(states.Keys()) == 0 {
		return nil, fmt.Errorf("machine has no states")
	}
	// Keys() preserves declaration order — linear-flow defaults depend on it.
	for _, name := range states.Keys() {
		if containsState(m.States, name) {
			return nil, fmt.Errorf("state %q declared twice", name)
		}
		st, err := l.state(name, l.obj(states.Get(name)))
		if err != nil {
			return nil, fmt.Errorf("state %q: %w", name, err)
		}
		m.States = append(m.States, st)
	}
	m.buildIndex()
	return m, nil
}

func containsState(states []*State, name string) bool {
	for _, s := range states {
		if s.Name == name {
			return true
		}
	}
	return false
}

func (l *loader) state(name string, o *goja.Object) (*State, error) {
	if o == nil {
		return nil, fmt.Errorf("state must be an object")
	}
	st := &State{
		Name:     name,
		Terminal: boolean(o.Get("terminal")),
		Status:   str(o.Get("status")),
		Memo:     boolean(o.Get("memo")),
	}

	if v := o.Get("agent"); defined(v) {
		ag, err := l.agent(v)
		if err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		st.Agent = ag
	}
	if v := o.Get("action"); defined(v) {
		st.Action = &ActionSpec{Name: v.String()}
	}
	if h := l.obj(o.Get("human")); h != nil {
		timeout, err := duration(h.Get("timeout"), "human.timeout")
		if err != nil {
			return nil, err
		}
		st.Human = &HumanSpec{
			Prompt:    l.dyn(h.Get("prompt")),
			Timeout:   timeout,
			OnTimeout: str(h.Get("onTimeout")),
		}
	}

	if f := l.obj(o.Get("forEach")); f != nil {
		st.ForEach = &ForEachSpec{
			Over:          l.dyn(f.Get("over")),
			As:            str(f.Get("as")),
			Concurrency:   integer(f.Get("concurrency")),
			OnItemFailure: str(f.Get("onItemFailure")),
		}
	}

	st.Input = l.dyn(o.Get("input"))

	if out := l.obj(o.Get("output")); out != nil {
		schema, err := l.exportData(out.Get("schema"), "output.schema")
		if err != nil {
			return nil, err
		}
		if schema != nil {
			sm, ok := schema.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("output.schema must be an object")
			}
			st.Output.Schema = sm
		}
		st.Output.Events = stringSlice(out.Get("events"))
	}

	if r, err := l.retries(o.Get("retry"), "retry"); err != nil {
		return nil, err
	} else if r != nil {
		st.Retry = r
	}

	if c := l.obj(o.Get("catch")); c != nil {
		n := int(c.Get("length").ToInteger())
		for i := 0; i < n; i++ {
			e := l.obj(c.Get(fmt.Sprintf("%d", i)))
			st.Catch = append(st.Catch, CatchClause{
				Match: stringSlice(e.Get("match")),
				To:    str(e.Get("to")),
			})
		}
	}

	if v := o.Get("transitions"); defined(v) {
		if _, isFn := goja.AssertFunction(v); isFn {
			return nil, fmt.Errorf("transitions must be data ({on, when, to}); only when: is a function")
		}
		if to, ok := v.Export().(string); ok {
			st.Transitions = []Transition{{To: to}}
		} else if t := l.obj(v); t != nil {
			n := int(t.Get("length").ToInteger())
			for i := 0; i < n; i++ {
				e := l.obj(t.Get(fmt.Sprintf("%d", i)))
				if e == nil {
					return nil, fmt.Errorf("transitions[%d] must be an object", i)
				}
				st.Transitions = append(st.Transitions, Transition{
					On:   str(e.Get("on")),
					When: l.dyn(e.Get("when")),
					To:   str(e.Get("to")),
				})
			}
		}
	}

	return st, nil
}

func (l *loader) agent(v goja.Value) (*AgentSpec, error) {
	// Shorthand: agent: "prompt" or agent: ({ctx}) => `...`
	if _, isFn := goja.AssertFunction(v); isFn {
		return &AgentSpec{Prompt: l.dyn(v)}, nil
	}
	if s, ok := v.Export().(string); ok {
		return &AgentSpec{Prompt: Dyn{Static: s}}, nil
	}
	o := l.obj(v)
	if o == nil {
		return nil, fmt.Errorf("agent must be a prompt, a function, or an object")
	}

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
			return nil, fmt.Errorf("adopt: object form requires from")
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
		for i := 0; i < n; i++ {
			tv := tools.Get(fmt.Sprintf("%d", i))
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
	for i := 0; i < n; i++ {
		e := l.obj(o.Get(fmt.Sprintf("%d", i)))
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
			cap, err := duration(b.Get("cap"), where+".backoff.cap")
			if err != nil {
				return nil, err
			}
			rp.Backoff = Backoff{
				Initial: initial,
				Factor:  b.Get("factor").ToFloat(),
				Jitter:  boolean(b.Get("jitter")),
				Cap:     cap,
			}
		}
		out = append(out, rp)
	}
	return out, nil
}
