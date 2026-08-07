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
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// resolveRuntimeAxes reads the run context for every from: axis of step and
// returns the values each one takes this run, keyed by var name.
//
// Values are STRINGS, exactly like a static values: list — `from:` is the same
// axis with the list computed at run time, so a cell interpolates
// `{{ .vars.x }}` and hashes identically either way. A source array of objects
// is refused rather than flattened: there is no rendering of an object into a
// name or a command that would not be a rule invented here.
func resolveRuntimeAxes(ctx context.Context, st *store.Store, runID string, step config.Step) (map[string][]string, error) {
	resolved := map[string][]string{}

	if !config.HasRuntimeAxis(step) {
		return resolved, nil
	}

	if st == nil || runID == "" {
		return nil, errors.New("across: from: needs the run context store, which this run has none of")
	}

	entries, err := st.RunContext(ctx, runID)
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
func decodeAxisValues(axis config.AcrossVar, byKey map[string]string) ([]string, error) {
	raw, ok := byKey[axis.From]
	if !ok {
		return nil, fmt.Errorf("across var %q takes its values from context key %q, which nothing in this run recorded", axis.Var, axis.From)
	}

	var values []string

	err := json.Unmarshal([]byte(raw), &values)
	if err != nil {
		return nil, fmt.Errorf("across var %q: context key %q must hold a JSON array of strings: %w", axis.Var, axis.From, err)
	}

	if len(values) == 0 {
		return nil, fmt.Errorf("across var %q: context key %q holds an empty array, so the matrix would run nothing at all", axis.Var, axis.From)
	}

	if len(values) > config.MaxAcrossItems {
		return nil, fmt.Errorf(
			"across var %q: context key %q holds %d items, above the limit of %d. Filter the array in the step that records it, or split the work across runs",
			axis.Var, axis.From, len(values), config.MaxAcrossItems)
	}

	return values, nil
}
