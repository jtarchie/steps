package main

import (
	"reflect"
	"testing"

	"github.com/jtarchie/steps/internal/config"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAnalyzer pins both halves: a tagless switch whose cases read a
// `kind:`-tagged field is reported (compound cases, mixed with modifiers,
// through embedding), and one that reads no kind field — or reads one only in
// a case body — is not, nor is one carrying a reasoned ignore directive.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "dispatch")
}

// The pass reports only when a selector resolves to a kind-tagged field, so a renamed or dropped tag on config.Step makes it exit 0 over a repo it checks nothing in. 12 tags exist today; the floor sits below that.
func TestKindTagStillExistsOnStep(t *testing.T) {
	typ := reflect.TypeFor[config.Step]()

	count := 0

	for field := range typ.Fields() {
		if _, ok := field.Tag.Lookup(kindTag); ok {
			count++
		}
	}

	if count < 10 {
		t.Fatalf("config.Step carries %d fields tagged `%s:`, want at least 10: the analyzer would find nothing to check and exit 0 as if the repo were clean", count, kindTag)
	}
}
