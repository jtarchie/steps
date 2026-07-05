package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"
)

// Compile resolves every output schema in the machine. Guards and prompts
// need no compilation — they are already JS functions; Validate dry-runs
// them against schema-derived stubs instead.
func Compile(m *Machine) error {
	var errs []error
	for _, s := range m.States {
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

// CompileOutputSchema turns the schema map (shorthand welcome — see
// schema.go) into a resolved JSON schema. Every declared property is
// required. When events are declared, an `event` enum property is injected
// and required — the agent-proposed event rides inside the output.
func CompileOutputSchema(props map[string]any, events []string) (*jsonschema.Resolved, error) {
	normalized, err := NormalizeSchema(props)
	if err != nil {
		return nil, err
	}
	properties := make(map[string]any, len(normalized)+1)
	required := make([]string, 0, len(normalized)+1)
	for k, v := range normalized {
		properties[k] = v
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
	err = json.Unmarshal(raw, &schema)
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	return schema.Resolve(nil)
}

// SchemaJSON renders the schema the model is asked to satisfy, for embedding
// in the output-contract instruction. Compact on purpose: the system
// instruction is re-sent on every model call, so every byte here is a
// recurring token cost.
func SchemaJSON(props map[string]any, events []string) (string, error) {
	normalized, err := NormalizeSchema(props)
	if err != nil {
		return "", err
	}
	properties := make(map[string]any, len(normalized)+1)
	for k, v := range normalized {
		properties[k] = v
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
	normalized, err := NormalizeSchema(props)
	if err != nil {
		normalized = props // compile already validated; defensive fallback
	}
	root := &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{},
	}
	keys := make([]string, 0, len(normalized)+1)
	for k := range normalized {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		root.Properties[k] = genaiProp(normalized[k])
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
