package machine

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ParseOption adjusts a machine between raw parsing and defaults expansion —
// the hook for engine-level defaults (the last rung of the cascade).
type ParseOption func(*Machine)

// WithEngineDefaultModel supplies the engine-level default model, used only
// when neither the state nor the machine's defaults block names one.
func WithEngineDefaultModel(model string) ParseOption {
	return func(m *Machine) {
		if m.Defaults.Agent.Model == "" {
			m.Defaults.Agent.Model = model
		}
	}
}

// Load reads, parses, expands, compiles, and validates a machine from a file.
func Load(path string, opts ...ParseOption) (*Machine, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(src, opts...)
}

// Parse builds a validated Machine from YAML bytes.
func Parse(src []byte, opts ...ParseOption) (*Machine, error) {
	m, err := parseRaw(src)
	if err != nil {
		return nil, err
	}
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
	return m, nil
}

// yamlMachine mirrors the top level of the file. States stay a raw node so
// document order is preserved (linear-flow defaults depend on it).
type yamlMachine struct {
	Version     int                      `yaml:"version"`
	Name        string                   `yaml:"name"`
	Description string                   `yaml:"description"`
	Input       map[string]yamlInputSpec `yaml:"input"`
	Defaults    yamlDefaults             `yaml:"defaults"`
	Limits      yamlLimits               `yaml:"limits"`
	Initial     string                   `yaml:"initial"`
	States      yaml.Node                `yaml:"states"`
}

type yamlInputSpec struct {
	Type     string `yaml:"type"`
	Required bool   `yaml:"required"`
}

type yamlDefaults struct {
	Agent yamlAgentDefaults `yaml:"agent"`
	Retry []yamlRetry       `yaml:"retry"`
}

type yamlAgentDefaults struct {
	Model       string   `yaml:"model"`
	Temperature *float64 `yaml:"temperature"`
	MaxTurns    int      `yaml:"max_turns"`
}

type yamlLimits struct {
	MaxTransitions int     `yaml:"max_transitions"`
	MaxCost        float64 `yaml:"max_cost"`
	MaxTokens      int     `yaml:"max_tokens"`
	Timeout        string  `yaml:"timeout"`
}

type yamlState struct {
	Agent       yaml.Node         `yaml:"agent"`
	Action      string            `yaml:"action"`
	Human       *yamlHuman        `yaml:"human"`
	Terminal    bool              `yaml:"terminal"`
	Status      string            `yaml:"status"`
	Input       map[string]string `yaml:"input"`
	Output      *yamlOutput       `yaml:"output"`
	Retry       yaml.Node         `yaml:"retry"`
	Catch       []yamlCatch       `yaml:"catch"`
	Transitions yaml.Node         `yaml:"transitions"`
}

type yamlAgent struct {
	Model       string       `yaml:"model"`
	System      string       `yaml:"system"`
	Prompt      string       `yaml:"prompt"`
	Tools       []yaml.Node  `yaml:"tools"`
	MaxTurns    int          `yaml:"max_turns"`
	Temperature *float64     `yaml:"temperature"`
	Adopt       string       `yaml:"adopt"`
	History     *yamlHistory `yaml:"history"`
	ToolChoice  string       `yaml:"tool_choice"`
}

type yamlToolRef struct {
	Name     string `yaml:"name"`
	MaxCalls int    `yaml:"max_calls"`
	When     string `yaml:"when"`
	OnReject string `yaml:"on_reject"`
	Require  string `yaml:"require"`
}

type yamlHistory struct {
	From      string   `yaml:"from"`
	Include   []string `yaml:"include"`
	LastTurns int      `yaml:"last_turns"`
	As        string   `yaml:"as"`
}

type yamlHuman struct {
	Prompt    string `yaml:"prompt"`
	Timeout   string `yaml:"timeout"`
	OnTimeout string `yaml:"on_timeout"`
}

type yamlOutput struct {
	Schema map[string]any `yaml:"schema"`
	Events []string       `yaml:"events"`
}

type yamlRetry struct {
	Match       []string    `yaml:"match"`
	MaxAttempts int         `yaml:"max_attempts"`
	Backoff     yamlBackoff `yaml:"backoff"`
}

type yamlBackoff struct {
	Initial string  `yaml:"initial"`
	Factor  float64 `yaml:"factor"`
	Jitter  bool    `yaml:"jitter"`
	Cap     string  `yaml:"cap"`
}

type yamlCatch struct {
	Match []string `yaml:"match"`
	To    string   `yaml:"to"`
}

type yamlTransition struct {
	On   string `yaml:"on"`
	When string `yaml:"when"`
	To   string `yaml:"to"`
}

func parseRaw(src []byte) (*Machine, error) {
	var ym yamlMachine
	if err := yaml.Unmarshal(src, &ym); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}

	m := &Machine{
		Version:     ym.Version,
		Name:        ym.Name,
		Description: ym.Description,
		Initial:     ym.Initial,
		RawYAML:     src,
		Hash:        hashBytes(src),
	}

	if len(ym.Input) > 0 {
		m.Input = make(map[string]InputSpec, len(ym.Input))
		for k, v := range ym.Input {
			m.Input[k] = InputSpec{Type: v.Type, Required: v.Required}
		}
	}

	m.Defaults.Agent = AgentDefaults{
		Model:       ym.Defaults.Agent.Model,
		Temperature: ym.Defaults.Agent.Temperature,
		MaxTurns:    ym.Defaults.Agent.MaxTurns,
	}
	for _, r := range ym.Defaults.Retry {
		rp, err := convertRetry(r)
		if err != nil {
			return nil, fmt.Errorf("defaults.retry: %w", err)
		}
		m.Defaults.Retry = append(m.Defaults.Retry, rp)
	}

	m.Limits = Limits{
		MaxTransitions: ym.Limits.MaxTransitions,
		MaxCost:        ym.Limits.MaxCost,
		MaxTokens:      ym.Limits.MaxTokens,
	}
	if ym.Limits.Timeout != "" {
		d, err := time.ParseDuration(ym.Limits.Timeout)
		if err != nil {
			return nil, fmt.Errorf("limits.timeout: %w", err)
		}
		m.Limits.Timeout = d
	}

	if ym.States.Kind == 0 {
		return nil, fmt.Errorf("machine has no states")
	}
	if ym.States.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("states must be a mapping")
	}
	// Mapping node content alternates key, value — this is what preserves
	// document order for the linear-flow default.
	for i := 0; i+1 < len(ym.States.Content); i += 2 {
		name := ym.States.Content[i].Value
		if m.State(name) != nil || containsState(m.States, name) {
			return nil, fmt.Errorf("state %q declared twice", name)
		}
		st, err := parseState(name, ym.States.Content[i+1])
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

func parseState(name string, node *yaml.Node) (*State, error) {
	var ys yamlState
	if err := node.Decode(&ys); err != nil {
		return nil, err
	}

	st := &State{
		Name:     name,
		Terminal: ys.Terminal,
		Status:   ys.Status,
		Input:    ys.Input,
	}

	if ys.Agent.Kind != 0 {
		ag, err := parseAgent(&ys.Agent)
		if err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		st.Agent = ag
	}
	if ys.Action != "" {
		st.Action = &ActionSpec{Name: ys.Action}
	}
	if ys.Human != nil {
		h := &HumanSpec{Prompt: ys.Human.Prompt, OnTimeout: ys.Human.OnTimeout}
		if ys.Human.Timeout != "" {
			d, err := time.ParseDuration(ys.Human.Timeout)
			if err != nil {
				return nil, fmt.Errorf("human.timeout: %w", err)
			}
			h.Timeout = d
		}
		st.Human = h
	}

	if ys.Output != nil {
		st.Output = OutputSpec{Schema: ys.Output.Schema, Events: ys.Output.Events}
	}

	// retry: absent (nil → engine/machine default), the string "none"
	// (→ empty, disables retries), or a list of policies.
	if ys.Retry.Kind == yaml.ScalarNode && ys.Retry.Value == "none" {
		st.Retry = []RetryPolicy{}
	} else if ys.Retry.Kind == yaml.SequenceNode {
		var yrs []yamlRetry
		if err := ys.Retry.Decode(&yrs); err != nil {
			return nil, fmt.Errorf("retry: %w", err)
		}
		st.Retry = make([]RetryPolicy, 0, len(yrs))
		for _, r := range yrs {
			rp, err := convertRetry(r)
			if err != nil {
				return nil, fmt.Errorf("retry: %w", err)
			}
			st.Retry = append(st.Retry, rp)
		}
	} else if ys.Retry.Kind != 0 {
		return nil, fmt.Errorf("retry must be a list or the string \"none\"")
	}

	for _, c := range ys.Catch {
		st.Catch = append(st.Catch, CatchClause{Match: c.Match, To: c.To})
	}

	// transitions: absent (linear default fills in), a scalar state name
	// shorthand, or a list of {on, when, to}.
	switch ys.Transitions.Kind {
	case 0:
		// leave nil; defaults fill in
	case yaml.ScalarNode:
		st.Transitions = []Transition{{To: ys.Transitions.Value}}
	case yaml.SequenceNode:
		var yts []yamlTransition
		if err := ys.Transitions.Decode(&yts); err != nil {
			return nil, fmt.Errorf("transitions: %w", err)
		}
		for _, t := range yts {
			st.Transitions = append(st.Transitions, Transition{On: t.On, When: t.When, To: t.To})
		}
	default:
		return nil, fmt.Errorf("transitions must be a list or a state name")
	}

	return st, nil
}

func parseAgent(node *yaml.Node) (*AgentSpec, error) {
	// Scalar shorthand: agent: "one-line prompt"
	if node.Kind == yaml.ScalarNode {
		return &AgentSpec{Prompt: node.Value}, nil
	}
	var ya yamlAgent
	if err := node.Decode(&ya); err != nil {
		return nil, err
	}
	ag := &AgentSpec{
		Model:       ya.Model,
		System:      ya.System,
		Prompt:      ya.Prompt,
		MaxTurns:    ya.MaxTurns,
		Temperature: ya.Temperature,
		Adopt:       ya.Adopt,
		ToolChoice:  ya.ToolChoice,
	}
	if ya.History != nil {
		ag.History = &HistorySpec{
			From:      ya.History.From,
			Include:   ya.History.Include,
			LastTurns: ya.History.LastTurns,
			As:        ya.History.As,
		}
	}
	for _, tn := range ya.Tools {
		if tn.Kind == yaml.ScalarNode {
			ag.Tools = append(ag.Tools, ToolRef{Name: tn.Value})
			continue
		}
		var yt yamlToolRef
		if err := tn.Decode(&yt); err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
		ag.Tools = append(ag.Tools, ToolRef{
			Name:     yt.Name,
			MaxCalls: yt.MaxCalls,
			When:     yt.When,
			OnReject: yt.OnReject,
			Require:  yt.Require,
		})
	}
	return ag, nil
}

func convertRetry(r yamlRetry) (RetryPolicy, error) {
	rp := RetryPolicy{Match: r.Match, MaxAttempts: r.MaxAttempts}
	if r.Backoff.Initial != "" {
		d, err := time.ParseDuration(r.Backoff.Initial)
		if err != nil {
			return rp, fmt.Errorf("backoff.initial: %w", err)
		}
		rp.Backoff.Initial = d
	}
	rp.Backoff.Factor = r.Backoff.Factor
	rp.Backoff.Jitter = r.Backoff.Jitter
	if r.Backoff.Cap != "" {
		d, err := time.ParseDuration(r.Backoff.Cap)
		if err != nil {
			return rp, fmt.Errorf("backoff.cap: %w", err)
		}
		rp.Backoff.Cap = d
	}
	return rp, nil
}
