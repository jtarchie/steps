package main

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAnalyzer pins both halves: a tagless switch whose cases read a
// `kind:`-tagged field is reported (compound cases, mixed with modifiers,
// through embedding), and one that reads no kind field — or reads one only in
// a case body — is not, nor is one carrying a reasoned ignore directive.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "dispatch")
}
