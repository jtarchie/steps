package machine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The schema shorthand exists for the human reading the YAML. All of these
// are equivalent to their JSON-schema expansions:
//
//	path: string                          {type: string}
//	risk: enum(low, medium, high)         {type: string, enum: [low, medium, high]}
//	tags: string[]                        {type: array, items: {type: string}}
//	leads: [{where: string, why: string}] {type: array, items: {type: object, ...}}
//	point: {x: number, y: number}         {type: object, properties: ..., required: all}
//
// Full JSON-schema fragments (any map containing a schema keyword like type,
// enum, items, maxItems...) pass through untouched, with nested values
// normalized recursively — shorthand and full form mix freely.

var scalarTypes = map[string]bool{
	"string": true, "number": true, "integer": true,
	"boolean": true, "object": true, "array": true,
}

var schemaKeywords = map[string]bool{
	"type": true, "enum": true, "items": true, "properties": true,
	"required": true, "maxItems": true, "minItems": true, "description": true,
	"maximum": true, "minimum": true, "maxLength": true, "minLength": true,
	"additionalProperties": true, "pattern": true, "format": true,
}

// NormalizeSchemaFragment expands one shorthand value into a JSON-schema map.
func NormalizeSchemaFragment(v any) (map[string]any, error) {
	switch t := v.(type) {
	case string:
		return normalizeScalar(t)
	case []any:
		if len(t) != 1 {
			return nil, fmt.Errorf("array shorthand takes exactly one item shape, got %d", len(t))
		}
		items, err := NormalizeSchemaFragment(t[0])
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case map[string]any:
		return normalizeMap(t)
	}
	return nil, fmt.Errorf("unsupported schema value %T (want a type name, enum(...), type[], [item shape], or a schema map)", v)
}

func normalizeScalar(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	if inner, ok := strings.CutPrefix(s, "enum("); ok && strings.HasSuffix(inner, ")") {
		// Both enum(a, b) and enum(a|b) work: pipes survive YAML flow
		// mappings ({severity: enum(a|b)}), where commas would be split
		// by YAML itself.
		var vals []any
		for _, part := range strings.FieldsFunc(strings.TrimSuffix(inner, ")"), func(r rune) bool {
			return r == ',' || r == '|'
		}) {
			if part = strings.TrimSpace(part); part != "" {
				vals = append(vals, part)
			}
		}
		if len(vals) == 0 {
			return nil, errors.New("enum() needs at least one value")
		}
		return map[string]any{"type": "string", "enum": vals}, nil
	}
	if inner, ok := strings.CutSuffix(s, "[]"); ok {
		items, err := normalizeScalar(inner)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	}
	if !scalarTypes[s] {
		return nil, fmt.Errorf("unknown type %q (want string, number, integer, boolean, object, array, enum(a, b), or type[])", s)
	}
	return map[string]any{"type": s}, nil
}

func normalizeMap(m map[string]any) (map[string]any, error) {
	isSchema := false
	for k := range m {
		if schemaKeywords[k] {
			isSchema = true
			break
		}
	}

	if !isSchema {
		// A bare object shape: {where: string, concern: string}.
		props := make(map[string]any, len(m))
		required := make([]string, 0, len(m))
		for k, v := range m {
			frag, err := NormalizeSchemaFragment(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			props[k] = frag
			required = append(required, k)
		}
		sort.Strings(required)
		return map[string]any{
			"type":                 "object",
			"properties":           props,
			"required":             required,
			"additionalProperties": true,
		}, nil
	}

	// A schema fragment: pass through, normalizing nested shorthand.
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	if t, ok := out["type"].(string); ok && !scalarTypes[t] {
		return nil, fmt.Errorf("unknown type %q", t)
	}
	if items, ok := out["items"]; ok {
		frag, err := NormalizeSchemaFragment(items)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		out["items"] = frag
	}
	if props, ok := out["properties"].(map[string]any); ok {
		normalized := make(map[string]any, len(props))
		for k, v := range props {
			frag, err := NormalizeSchemaFragment(v)
			if err != nil {
				return nil, fmt.Errorf("properties.%s: %w", k, err)
			}
			normalized[k] = frag
		}
		out["properties"] = normalized
	}
	return out, nil
}

// NormalizeSchema expands every property of an output schema.
func NormalizeSchema(props map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(props))
	for k, v := range props {
		frag, err := NormalizeSchemaFragment(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = frag
	}
	return out, nil
}
