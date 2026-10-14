package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/jtarchie/steps/internal/config"
)

// A tool's params: limit what each argument may be. They are published in the
// tool's input schema, where a model reads them, and enforced in front of the
// impl, where a caller that ignored them is refused all the same — the
// schema is advice, the wrapper is the rule.

// paramLimit is one argument's limits with the type the tool declares for it.
type paramLimit struct {
	name    string
	limits  config.ParamSpec
	numeric bool
	integer bool
	// fill is the typed default, nil when there is none.
	fill any
}

// limited publishes spec.Params into the declaration and refuses a call that breaks them. It runs at build time on the impls, like bound, so a CLI-backed step's bridge and an export serve the same limits.
func (r resolvedSpec) limited(spec config.ToolSpec) error {
	if len(spec.Params) == 0 {
		return nil
	}

	if len(r.decls) != 1 {
		return fmt.Errorf("agent tool %q: params: needs a tool that declares one function", config.ToolSpecName(spec))
	}

	decl := r.decls[0]

	schema, props := copySchema(declInputSchema(decl))
	if schema == nil {
		return fmt.Errorf("tool %q: params: the tool's input schema cannot be read", decl.Name)
	}

	if props == nil {
		props = map[string]any{}
	}

	limits := make([]paramLimit, 0, len(spec.Params))

	for _, name := range sortedKeys(spec.Params) {
		prop, ok := props[name].(map[string]any)
		if !ok {
			return fmt.Errorf("tool %q: params: %q is not an argument it takes (it takes: %s)", decl.Name, name, quoteJoin(sortedKeys(props)))
		}

		limit, err := publishParam(decl.Name, name, spec.Params[name], prop)
		if err != nil {
			return err
		}

		if limit.fill != nil {
			schema["required"] = dropRequired(schema["required"], name)
		}

		limits = append(limits, limit)
	}

	decl.Parameters = nil
	decl.ParametersJsonSchema = schema
	r.impls[decl.Name] = withParamLimits(decl.Name, limits, r.impls[decl.Name])

	return nil
}

func dropRequired(required any, name string) any {
	list, ok := required.([]any)
	if !ok {
		return required
	}

	return slices.DeleteFunc(list, func(entry any) bool { return entry == name })
}

// publishParam writes one argument's limits into its schema property and returns what the wrapper enforces.
func publishParam(tool, name string, limits config.ParamSpec, prop map[string]any) (paramLimit, error) {
	limit, err := newParamLimit(tool, name, limits, prop)
	if err != nil {
		return paramLimit{}, err
	}

	publishBounds(prop, limits)

	err = publishEnumAndDefault(tool, &limit, prop)
	if err != nil {
		return paramLimit{}, err
	}

	return limit, nil
}

// newParamLimit reads the argument's type from its schema property and refuses limits that type cannot have.
func newParamLimit(tool, name string, limits config.ParamSpec, prop map[string]any) (paramLimit, error) {
	kind, _ := prop["type"].(string)
	if kind == "" {
		kind = "string"
	}

	limit := paramLimit{name: name, limits: limits, numeric: kind == "integer" || kind == "number", integer: kind == "integer"}

	switch {
	case !limit.numeric && kind != "string":
		return paramLimit{}, fmt.Errorf("tool %q: params: %q is a %s, and only a string or a number can be limited", tool, name, kind)
	case limit.numeric && limits.StringLimits():
		return paramLimit{}, fmt.Errorf("tool %q: params: %q is a %s, so max_length/min_length/pattern do not apply — use min/max", tool, name, kind)
	case !limit.numeric && limits.NumberLimits():
		return paramLimit{}, fmt.Errorf("tool %q: params: %q is a string, so min/max do not apply — use max_length/min_length", tool, name)
	}

	return limit, nil
}

func publishBounds(prop map[string]any, limits config.ParamSpec) {
	if limits.MaxLength != nil {
		prop["maxLength"] = *limits.MaxLength
	}

	if limits.MinLength != nil {
		prop["minLength"] = *limits.MinLength
	}

	if limits.Pattern != "" {
		prop["pattern"] = limits.Pattern
	}

	if limits.Min != nil {
		prop["minimum"] = *limits.Min
	}

	if limits.Max != nil {
		prop["maximum"] = *limits.Max
	}
}

func publishEnumAndDefault(tool string, limit *paramLimit, prop map[string]any) error {
	limits, name := limit.limits, limit.name

	if len(limits.Enum) > 0 {
		values := make([]any, len(limits.Enum))

		for i, entry := range limits.Enum {
			typed, err := typedValue(*limit, entry)
			if err != nil {
				return fmt.Errorf("tool %q: params: %q: enum entry %q: %w", tool, name, entry, err)
			}

			values[i] = typed
		}

		prop["enum"] = values
	}

	if limits.Default == nil {
		return nil
	}

	typed, err := typedValue(*limit, *limits.Default)
	if err != nil {
		return fmt.Errorf("tool %q: params: %q: default: %w", tool, name, err)
	}

	if why := checkParam(*limit, typed); why != "" {
		return fmt.Errorf("tool %q: params: %q: the default %s", tool, name, why)
	}

	prop["default"] = typed
	limit.fill = typed

	return nil
}

func typedValue(limit paramLimit, text string) (any, error) {
	if !limit.numeric {
		return text, nil
	}

	n, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", text)
	}

	if limit.integer {
		if n != float64(int64(n)) {
			return nil, fmt.Errorf("%q is not a whole number", text)
		}

		return int64(n), nil
	}

	return n, nil
}

// checkParam is why value breaks limit, or "". The value is never quoted back: it is whatever the caller pasted.
func checkParam(limit paramLimit, value any) string {
	if limit.numeric {
		n, ok := numberValue(value)
		if !ok {
			return "must be a number"
		}

		if limit.integer && n != float64(int64(n)) {
			return "must be a whole number"
		}

		return limit.limits.CheckNumber(n)
	}

	text, ok := value.(string)
	if !ok {
		return "must be a string"
	}

	return limit.limits.CheckString(text)
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()

		return n, err == nil
	default:
		return 0, false
	}
}

func withParamLimits(tool string, limits []paramLimit, impl toolImpl) toolImpl {
	return func(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
		filled := make(map[string]any, len(args)+len(limits))
		for key, value := range args {
			filled[key] = value
		}

		for _, limit := range limits {
			if value, given := filled[limit.name]; !given || value == nil {
				if limit.fill != nil {
					filled[limit.name] = limit.fill
				}

				continue
			}

			if why := checkParam(limit, filled[limit.name]); why != "" {
				return map[string]any{"error": fmt.Sprintf("%s: %s %s", tool, limit.name, why)}
			}
		}

		return impl(ctx, filled, env)
	}
}
