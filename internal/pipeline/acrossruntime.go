package pipeline

// Runtime fan-out: an across: axis whose values come from the run context
// rather than from the pipeline text (`from: <key>`).
//
// This is "the agent plans, the pipeline executes": one step records a JSON
// array of things to work through, and the matrix runs a cell per item — each
// cell independently hashed, cached, and reported, instead of one agent
// grinding through the whole list in a conversation that outgrows its window.
//
// The array is produced during the run, usually by a model, so nothing about
// it was reviewed by the pipeline author. Everything here treats it as such.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// resolveRuntimeAxes reads the run context for every from: axis of step and
// returns the values each one takes this run, keyed by var name.
//
// An array holds STRINGS, exactly like a static values: list, or flat OBJECTS
// whose fields a cell names individually (`{{ .vars.finding.file }}`). Either
// way a cell renders to text and hashes identically to the static cell it is
// indistinguishable from — `from:` is the same axis with the list computed at
// run time, not a different kind of axis.
//
// What is still refused is a rendering nobody asked for: mixed arrays, nested
// values inside an item, and (in internal/config) an object interpolated
// without naming a field.
// scopes is the run plus whatever concurrent blocks this matrix sits inside,
// layered nearest-wins — the same set an agent step's recap reads, so a matrix
// can fan out over an array its own branch recorded rather than only over one
// that existed before the block.
func resolveRuntimeAxes(ctx context.Context, st *store.Store, scopes []string, step config.Step) (map[string][]any, error) {
	resolved := map[string][]any{}

	if !config.HasRuntimeAxis(step) {
		return resolved, nil
	}

	if st == nil || len(scopes) == 0 || scopes[0] == "" {
		return nil, errors.New("across: from: needs the run context store, which this run has none of")
	}

	entries, err := st.LayeredContext(ctx, scopes)
	if err != nil {
		return nil, fmt.Errorf("across: %w", err)
	}

	byKey := make(map[string]string, len(entries))
	for _, entry := range entries {
		byKey[entry.Key] = entry.Value
	}

	for _, axis := range step.Across {
		if !axis.Runtime() {
			continue
		}

		values, err := decodeAxisValues(axis, byKey)
		if err != nil {
			return nil, err
		}

		resolved[axis.Var] = values
	}

	return resolved, nil
}

// decodeAxisValues turns one recorded context value into an axis's values.
//
// Every failure names the key and says what was found, because the author is
// debugging a step they did not write the output of: "your model produced the
// wrong shape" is only actionable if the message says which key and which
// shape.
func decodeAxisValues(axis config.AcrossVar, byKey map[string]string) ([]any, error) {
	raw, ok := byKey[axis.From]
	if !ok {
		return nil, fmt.Errorf("across var %q takes its values from context key %q, which nothing in this run recorded", axis.Var, axis.From)
	}

	// UseNumber keeps a number's own text, so `line: 42` renders "42" rather
	// than float64's "42" via a formatting rule this code would be choosing.
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()

	var items []json.RawMessage

	err := decoder.Decode(&items)
	if err != nil {
		return nil, fmt.Errorf("across var %q: context key %q must hold a JSON array of strings or of flat objects: %w", axis.Var, axis.From, err)
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("across var %q: context key %q holds an empty array, so the matrix would run nothing at all", axis.Var, axis.From)
	}

	if len(items) > config.MaxAcrossItems {
		return nil, fmt.Errorf(
			"across var %q: context key %q holds %d items, above the limit of %d. Filter the array in the step that records it, or split the work across runs",
			axis.Var, axis.From, len(items), config.MaxAcrossItems)
	}

	return decodeAxisItems(axis, items)
}

// decodeAxisItems decodes every item of an axis's array, requiring them all to
// be the same shape.
//
// Homogeneity is required rather than accommodated: a mixed array means the
// step that recorded it disagreed with itself about what an item is, and half
// the cells would then render a template the other half cannot. Refusing names
// the item that broke the pattern, which is the one a model has to be told
// about.
func decodeAxisItems(axis config.AcrossVar, items []json.RawMessage) ([]any, error) {
	values := make([]any, 0, len(items))

	var objects bool

	for i, raw := range items {
		value, isObject, err := decodeAxisItem(axis, i, raw)
		if err != nil {
			return nil, err
		}

		if i == 0 {
			objects = isObject
		} else if isObject != objects {
			return nil, fmt.Errorf("across var %q: context key %q mixes shapes — item %d is %s but item 0 is %s; every item must be the same shape",
				axis.Var, axis.From, i, itemShape(isObject), itemShape(objects))
		}

		values = append(values, value)
	}

	return values, nil
}

// decodeAxisItem decodes one item: a JSON string, or a flat object whose
// fields are all scalars.
//
// A nested object or array inside an item is refused rather than rendered.
// `{{ .vars.finding.files }}` over a list has no text it obviously becomes, and
// picking one here (comma-joined? JSON?) would be exactly the invented rule
// objects were originally kept out of from: to avoid. Record the nested part as
// its own key, or flatten it in the step that writes it.
func decodeAxisItem(axis config.AcrossVar, index int, raw json.RawMessage) (any, bool, error) {
	var text string

	if json.Unmarshal(raw, &text) == nil {
		return text, false, nil
	}

	fields := map[string]any{}

	// UseNumber again here, so a field keeps the number's own text: `line: 42`
	// must render "42", not float64's formatting of it.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	err := decoder.Decode(&fields)
	if err != nil {
		return nil, false, fmt.Errorf("across var %q: context key %q item %d is neither a string nor an object; an array holds one or the other",
			axis.Var, axis.From, index)
	}

	flat := make(map[string]string, len(fields))

	for _, name := range sortedAnyKeys(fields) {
		scalar, ok := scalarText(fields[name])
		if !ok {
			return nil, false, fmt.Errorf("across var %q: context key %q item %d field %q holds a %s; an item's fields must be strings, numbers or booleans, since each one renders into a command or a prompt",
				axis.Var, axis.From, index, name, jsonKind(fields[name]))
		}

		flat[name] = scalar
	}

	if len(flat) == 0 {
		return nil, false, fmt.Errorf("across var %q: context key %q item %d is an empty object, so its cell would have nothing to interpolate",
			axis.Var, axis.From, index)
	}

	return flat, true, nil
}

// scalarText renders one decoded JSON value as the text a template will
// substitute, reporting false for the composite kinds that have no such text.
func scalarText(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case bool:
		return strconv.FormatBool(typed), true
	default:
		return "", false
	}
}

// jsonKind names a value's JSON kind for an error message.
func jsonKind(value any) string {
	switch value.(type) {
	case map[string]any:
		return "nested object"
	case []any:
		return "list"
	case nil:
		return "null"
	default:
		return "value of an unsupported kind"
	}
}

// itemShape names an item's shape for the mixed-array error.
func itemShape(object bool) string {
	if object {
		return "an object"
	}

	return "a string"
}

// sortedAnyKeys returns m's keys in sorted order, so a malformed item is
// reported by naming the same field on every run.
func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
