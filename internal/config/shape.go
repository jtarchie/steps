package config

// assert.shape: the structural type an agent or task step's JSON output must
// have. It is the one part of assert.files' sibling contract that files alone
// cannot express: a file can exist, be non-empty, and still be the wrong
// SHAPE — the replanner that wrote {intents:[...]} where a bare [ids] was
// owed, and sailed past assert.files because the file was there. Shape is
// STRUCTURAL ONLY (it pins a value's JSON kind, and an array's element kind,
// never a value), which is what keeps it a nudgeable obligation rather than a
// smuggled conclusion: told the shape is wrong, a model cannot read the answer
// off the telling.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// shapeScalars are the JSON kinds a shape expr can name, each a predicate over
// a value decoded by encoding/json (a number is float64, an object a
// map[string]any, an array a []any).
var shapeScalars = map[string]func(any) bool{
	"string":  func(v any) bool { _, ok := v.(string); return ok },
	"number":  func(v any) bool { _, ok := v.(float64); return ok },
	"boolean": func(v any) bool { _, ok := v.(bool); return ok },
	"object":  func(v any) bool { _, ok := v.(map[string]any); return ok },
	"array":   func(v any) bool { _, ok := v.([]any); return ok },
}

// arrayElem splits array<KIND> into KIND; the bool says whether expr had that
// form at all.
func arrayElem(expr string) (string, bool) {
	if strings.HasPrefix(expr, "array<") && strings.HasSuffix(expr, ">") {
		return expr[len("array<") : len(expr)-1], true
	}

	return "", false
}

// shapeKindList is the scalar kinds in sorted order, for an error message.
func shapeKindList() string {
	kinds := make([]string, 0, len(shapeScalars))
	for k := range shapeScalars {
		kinds = append(kinds, k)
	}

	sort.Strings(kinds)

	return strings.Join(kinds, ", ")
}

// ValidShapeExpr accepts a scalar kind or array<KIND> for one of them, and
// rejects everything else at load — one level of nesting, because a deeper
// grammar buys little over the downstream step's own gate and a value matcher
// would turn the obligation into a conclusion.
func ValidShapeExpr(expr string) error {
	if _, ok := shapeScalars[expr]; ok {
		return nil
	}

	if elem, ok := arrayElem(expr); ok {
		if _, ok := shapeScalars[elem]; ok {
			return nil
		}

		return fmt.Errorf("shape %q: array element kind %q is not one of %s", expr, elem, shapeKindList())
	}

	return fmt.Errorf("shape %q is not a known kind; use one of %s, or array<KIND>", expr, shapeKindList())
}

// shapeMatches reports whether v (decoded by encoding/json) has the structure
// expr names. expr is assumed already accepted by ValidShapeExpr.
func shapeMatches(expr string, v any) bool {
	if pred, ok := shapeScalars[expr]; ok {
		return pred(v)
	}

	elem, _ := arrayElem(expr)

	arr, ok := v.([]any)
	if !ok {
		return false
	}

	pred := shapeScalars[elem]
	for _, e := range arr {
		if !pred(e) {
			return false
		}
	}

	return true
}

// jsonKind names a decoded value's kind for a mismatch message.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// AssertShapeMismatch reports the first assert.shape entry not satisfied under
// dir, or nil when every one checks out.
func AssertShapeMismatch(shape map[string]string, dir string) error {
	unmet := AssertShapeMismatches(shape, dir)
	if len(unmet) == 0 {
		return nil
	}

	return fmt.Errorf("assert.shape: %s", unmet[0])
}

// AssertShapeMismatches reports EVERY unmet assert.shape entry — missing,
// empty, a directory, not valid JSON, or not the declared shape — each phrased
// as the reason alone so the agent path can hand them to the model one message
// at a time and the task path can join them. Keys are checked in sorted order
// so the reasons, and any nudge built from them, are deterministic.
func AssertShapeMismatches(shape map[string]string, dir string) []string {
	paths := make([]string, 0, len(shape))
	for rel := range shape {
		paths = append(paths, rel)
	}

	sort.Strings(paths)

	var unmet []string

	for _, rel := range paths {
		reason := shapeMismatch(rel, shape[rel], dir)
		if reason != "" {
			unmet = append(unmet, reason)
		}
	}

	return unmet
}

// shapeMismatch is the per-entry check: the reason one file fails its shape,
// or "" when it passes.
func shapeMismatch(rel, expr, dir string) string {
	info, err := os.Stat(filepath.Join(dir, rel))

	switch {
	case err != nil:
		return rel + " does not exist"
	case info.IsDir():
		return rel + " is a directory, not a file"
	case info.Size() == 0:
		return rel + " is empty"
	}

	data, err := os.ReadFile(filepath.Join(dir, rel)) //nolint:gosec // rel is a load-validated artifact-relative path, dir is the step workspace
	if err != nil {
		return rel + " could not be read"
	}

	var v any

	err = json.Unmarshal(data, &v)
	if err != nil {
		return rel + " is not valid JSON"
	}

	if !shapeMatches(expr, v) {
		return fmt.Sprintf("%s is not %s (got %s)", rel, expr, jsonKind(v))
	}

	return ""
}
