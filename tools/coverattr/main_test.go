package main

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, text string) profile {
	t.Helper()

	p, err := parseProfile(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// The three answers that matter: a block the package tests execute is nobody's; a block only one e2e test executes is that test's alone; a block two do is e2e-only but unique to neither.
func TestAttributeSeparatesPackageSharedAndUniqueBlocks(t *testing.T) { //nolint:cyclop // one check per answer the attribution gives
	t.Parallel()

	pkg := mustParse(t, `mode: set
github.com/jtarchie/steps/internal/a/a.go:1.1,2.1 3 1
github.com/jtarchie/steps/internal/a/a.go:3.1,4.1 5 0
github.com/jtarchie/steps/internal/b/b.go:1.1,2.1 7 0
github.com/jtarchie/steps/internal/b/b.go:5.1,6.1 11 0
github.com/jtarchie/steps/internal/a/a.go:3.1,4.1 5 0
`)

	tests := map[string]profile{
		"TestOne": mustParse(t, `mode: set
github.com/jtarchie/steps/internal/a/a.go:1.1,2.1 3 1
github.com/jtarchie/steps/internal/a/a.go:3.1,4.1 5 1
github.com/jtarchie/steps/internal/b/b.go:1.1,2.1 7 1
`),
		"TestTwo": mustParse(t, `mode: set
github.com/jtarchie/steps/internal/b/b.go:1.1,2.1 7 1
`),
	}

	report := attribute(pkg, tests, map[string]float64{"TestOne": 2, "TestTwo": 1})

	if report.Statements != 26 || report.PkgCovered != 3 || report.E2EOnly != 12 || report.BothCovered != 15 {
		t.Fatalf("totals = %+v, want 26 statements, 3 package, 12 e2e-only, 15 either", report)
	}

	byName := map[string]Test{}
	for _, test := range report.Tests {
		byName[test.Name] = test
	}

	if one := byName["TestOne"]; one.Unique != 5 || one.E2EOnly != 12 || one.Where["internal/a"] != 5 {
		t.Errorf("TestOne = %+v, want 5 unique (a.go:3) of its 12 e2e-only", one)
	}

	if two := byName["TestTwo"]; two.Unique != 0 || two.E2EOnly != 7 {
		t.Errorf("TestTwo = %+v, want 7 e2e-only, none unique: TestOne executes b.go:1 too", two)
	}

	if len(report.Blocks) != 2 || strings.Join(report.Blocks[0].Tests, ",") != "TestOne" || strings.Join(report.Blocks[1].Tests, ",") != "TestOne,TestTwo" {
		t.Errorf("blocks = %+v, want a.go:3 held by TestOne and b.go:1 by both", report.Blocks)
	}
}
