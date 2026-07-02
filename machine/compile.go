package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/google/jsonschema-go/jsonschema"
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
// in the output-contract instruction.
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
	raw, err := json.MarshalIndent(map[string]any{"type": "object", "properties": properties}, "", "  ")
	return string(raw), err
}
