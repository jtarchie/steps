package main

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAnalyzer runs the pass over testdata/src/dispatch, whose `// want`
// comments pin both halves of the contract: it fires on a tagless switch that
// omits a kind from the type's own Kind() table, and stays silent on the
// tagless switches that are not kind dispatch at all — ordinary boolean
// switches, ones mixing a kind field with something else, and ones over two
// variables. The second half is the reason this analyzer is scoped the way it
// is: a checker that fired on every tagless switch would be turned off.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "dispatch")
}
