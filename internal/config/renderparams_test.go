package config

import "testing"

// A put's cache key carries params: unconditionally, so a rendered step with none must hand it {} as it always has: null would move every such put's key.
func TestRenderLocalVarsKeepsAnAbsentParamsAnEmptyMapping(t *testing.T) {
	t.Parallel()

	rendered, err := Step{Put: "release", Timeout: "((.:wait))"}.RenderLocalVars(func(LocalRef) (string, error) { return "1m", nil })
	if err != nil {
		t.Fatalf("RenderLocalVars: %v", err)
	}

	if rendered.Params == nil {
		t.Error("Params = nil, want an empty mapping")
	}
}
