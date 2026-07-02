package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"
)

// GuardEnv is the shape guards compile against. Compilation catches unknown
// top-level identifiers (`outputs.confidence` fails at load); member access on
// the maps stays dynamic and is checked at evaluation time.
func GuardEnv() map[string]any {
	return map[string]any{
		"ctx":     map[string]any{},
		"output":  map[string]any{},
		"event":   "",
		"attempt": 0,
		"visits":  map[string]int{},
		"run":     map[string]any{"transitions": 0, "tokens": 0, "cost": 0.0},
		"state":   map[string]any{"elapsed": 0.0},
		// tool-guard additions
		"args":  map[string]any{},
		"calls": map[string]int{},
		"turn":  0,
	}
}

// CompileGuard compiles an Expr guard against the guard environment.
func CompileGuard(src string) (*vm.Program, error) {
	return expr.Compile(src, expr.Env(GuardEnv()), expr.AsBool())
}

// CompileExpr compiles a non-boolean Expr (e.g. foreach.over) against the
// same environment.
func CompileExpr(src string) (*vm.Program, error) {
	return expr.Compile(src, expr.Env(GuardEnv()))
}

// EvalExpr evaluates a compiled non-boolean Expr.
func EvalExpr(p *vm.Program, env map[string]any) (any, error) {
	return expr.Run(p, env)
}

// EvalGuard evaluates a compiled guard. Errors (e.g. a missing output field)
// are returned so the engine can treat the guard as false with a warning.
func EvalGuard(p *vm.Program, env map[string]any) (bool, error) {
	out, err := expr.Run(p, env)
	if err != nil {
		return false, err
	}
	b, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("guard returned %T, want bool", out)
	}
	return b, nil
}

// Elapsed converts a duration for the guard env (seconds as float).
func Elapsed(d time.Duration) float64 { return d.Seconds() }

// Compile compiles every guard and output schema in the machine.
// It must run after ApplyDefaults and before Validate.
func Compile(m *Machine) error {
	var errs []error
	for _, s := range m.States {
		for i := range s.Transitions {
			t := &s.Transitions[i]
			if t.When == "" {
				continue
			}
			p, err := CompileGuard(t.When)
			if err != nil {
				errs = append(errs, fmt.Errorf("state %q transition %d: guard %q: %w", s.Name, i, t.When, err))
				continue
			}
			t.Guard = p
		}
		if s.Agent != nil {
			for i := range s.Agent.Tools {
				tr := &s.Agent.Tools[i]
				if tr.When == "" {
					continue
				}
				p, err := CompileGuard(tr.When)
				if err != nil {
					errs = append(errs, fmt.Errorf("state %q tool %q: guard %q: %w", s.Name, tr.Name, tr.When, err))
					continue
				}
				tr.Guard = p
			}
		}
		if f := s.ForEach; f != nil && f.Over != "" {
			p, err := CompileExpr(f.Over)
			if err != nil {
				errs = append(errs, fmt.Errorf("state %q foreach.over %q: %w", s.Name, f.Over, err))
			} else {
				f.Program = p
			}
		}
		if len(s.Output.Schema) > 0 {
			resolved, err := CompileOutputSchema(s.Output.Schema, s.Output.Events)
			if err != nil {
				errs = append(errs, fmt.Errorf("state %q output schema: %w", s.Name, err))
				continue
			}
			s.Output.Compiled = resolved
		}
	}
	return errors.Join(errs...)
}

// CompileOutputSchema turns the YAML property map into a resolved JSON schema.
// Scalar shorthand (`severity: string`) becomes {type: string}. Every declared
// property is required. When events are declared, an `event` enum property is
// injected and required — the agent-proposed event rides inside the output.
func CompileOutputSchema(props map[string]any, events []string) (*jsonschema.Resolved, error) {
	properties := make(map[string]any, len(props)+1)
	required := make([]string, 0, len(props)+1)
	for k, v := range props {
		if ts, ok := v.(string); ok {
			properties[k] = map[string]any{"type": ts}
		} else {
			properties[k] = v
		}
		required = append(required, k)
	}
	if len(events) > 0 {
		evs := make([]any, len(events))
		for i, e := range events {
			evs[i] = e
		}
		properties["event"] = map[string]any{"type": "string", "enum": evs}
		required = append(required, "event")
	}
	full := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": true,
	}
	raw, err := json.Marshal(full)
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	return schema.Resolve(nil)
}

// SchemaJSON renders the schema the model is asked to satisfy, for embedding
// in the output-contract instruction. Compact on purpose: the system
// instruction is re-sent on every model call, so every byte here is a
// recurring token cost.
func SchemaJSON(props map[string]any, events []string) (string, error) {
	properties := make(map[string]any, len(props)+1)
	for k, v := range props {
		if ts, ok := v.(string); ok {
			properties[k] = map[string]any{"type": ts}
		} else {
			properties[k] = v
		}
	}
	if len(events) > 0 {
		properties["event"] = map[string]any{"type": "string", "enum": events}
	}
	raw, err := json.Marshal(map[string]any{"type": "object", "properties": properties})
	return string(raw), err
}

// GenaiSchema converts the state's output contract into a *genai.Schema so
// providers with native structured output (OpenAI-compatible: LM Studio,
// Ollama, OpenAI) constrain the decoder itself — no preamble tokens, no
// malformed JSON, and most semantic retries never happen. Providers without
// support ignore it; the prompt contract remains the portable fallback.
func GenaiSchema(props map[string]any, events []string) *genai.Schema {
	root := &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{},
	}
	keys := make([]string, 0, len(props)+1)
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		root.Properties[k] = genaiProp(props[k])
		root.Required = append(root.Required, k)
	}
	if len(events) > 0 {
		root.Properties["event"] = &genai.Schema{Type: genai.TypeString, Enum: events}
		root.Required = append(root.Required, "event")
	}
	return root
}

func genaiProp(v any) *genai.Schema {
	if ts, ok := v.(string); ok {
		return &genai.Schema{Type: genaiType(ts)}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return &genai.Schema{Type: genai.TypeString}
	}
	s := &genai.Schema{}
	if t, ok := m["type"].(string); ok {
		s.Type = genaiType(t)
	}
	if d, ok := m["description"].(string); ok {
		s.Description = d
	}
	if enum, ok := m["enum"].([]any); ok {
		s.Type = genai.TypeString
		for _, e := range enum {
			s.Enum = append(s.Enum, fmt.Sprintf("%v", e))
		}
	}
	if items, ok := m["items"]; ok {
		if s.Type == genai.TypeUnspecified {
			s.Type = genai.TypeArray
		}
		s.Items = genaiProp(items)
	}
	if props, ok := m["properties"].(map[string]any); ok {
		if s.Type == genai.TypeUnspecified {
			s.Type = genai.TypeObject
		}
		s.Properties = map[string]*genai.Schema{}
		for k, pv := range props {
			s.Properties[k] = genaiProp(pv)
			s.Required = append(s.Required, k)
		}
		sort.Strings(s.Required)
	}
	if n, ok := asInt64(m["minItems"]); ok {
		s.MinItems = genai.Ptr(n)
	}
	if n, ok := asInt64(m["maxItems"]); ok {
		s.MaxItems = genai.Ptr(n)
	}
	if s.Type == genai.TypeUnspecified {
		s.Type = genai.TypeString
	}
	return s
}

func genaiType(t string) genai.Type {
	switch t {
	case "string":
		return genai.TypeString
	case "number":
		return genai.TypeNumber
	case "integer":
		return genai.TypeInteger
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case "object":
		return genai.TypeObject
	}
	return genai.TypeString
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}
