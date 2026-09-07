package agent

// Attestation: `--tools ""` (see cliexec.go) is policy inside an upstream
// binary this build does not pin, whose built-in surface grows with releases.
// The stream-json `init` event lists the session's tools, so this package
// checks it against exactly what was bridged and refuses to trust a child
// that disagrees.
//
// This is DETECTION, not prevention: the check runs after init is parsed, so
// a surplus native could in principle be called in the child's first turn
// before this package reacts. `--tools ""` remains the primary fence;
// attestation is the per-run proof that it held.

import (
	"errors"
	"fmt"
	"slices"
)

// errCLIToolSurface is the sentinel an attestation failure wraps. A plain
// error, not outcome.Fail: steps refusing to trust the child is an
// infrastructure condition (fires on_error), never the child's own answer to
// its task, so retrying it is deterministic waste — see cli.go's
// retry.Stop(errCLIToolSurface) on this sentinel.
var errCLIToolSurface = errors.New("cli tool surface attestation failed")

// checkCLIToolSurface compares what the child's init event reported against
// expected — the exact bridged grant (cliToolPermissions) — and returns
// errCLIToolSurface on ANY disagreement, not merely a surplus: set equality,
// not a subset check. This subsumes "the bridge never connected" for free
// (expected non-empty, reported empty reads as every expected tool missing).
//
// Both slices are sorted copies; callers pass their own slices unmodified.
func checkCLIToolSurface(reported, expected []string) error {
	reported = slices.Clone(reported)
	expected = slices.Clone(expected)

	slices.Sort(reported)
	slices.Sort(expected)

	if slices.Equal(reported, expected) {
		return nil
	}

	surplus := setDifference(reported, expected)
	missing := setDifference(expected, reported)

	if len(surplus) > 0 {
		return fmt.Errorf("%w: the cli reported tool(s) %s beyond the %s this build granted — "+
			"upgrade or downgrade the cli to a version whose init event matches its --tools/--allowedTools argv, "+
			"or file a bug if this is a supported version",
			errCLIToolSurface, quoteJoin(surplus), describeExpected(expected))
	}

	return fmt.Errorf("%w: the cli's init event did not report granted tool(s) %s — "+
		"check that the cli's minimum supported version reports `tools` on its stream-json init event",
		errCLIToolSurface, quoteJoin(missing))
}

// describeExpected renders the grant a surplus is measured against, or says
// there was none — a step granted no tools at all still owes an init event
// reporting an empty list, and "beyond the (nothing) this build granted"
// reads oddly without this.
func describeExpected(expected []string) string {
	if len(expected) == 0 {
		return "empty grant"
	}

	return quoteJoin(expected)
}

// setDifference returns the members of a not present in b, in a's order.
func setDifference(a, b []string) []string {
	present := make(map[string]bool, len(b))
	for _, name := range b {
		present[name] = true
	}

	var diff []string

	for _, name := range a {
		if !present[name] {
			diff = append(diff, name)
		}
	}

	return diff
}
