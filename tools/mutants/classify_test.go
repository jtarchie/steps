package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileFailureOnlyScoresTheCompilersRejectionAsAKill(t *testing.T) {
	cases := map[string]bool{
		"# fix\nfix.go:5:9: invalid operation: operator - not defined on s (variable of type string)\n": true,
		"# fix\nfix.go:5:9: cannot use 1 (untyped int constant) as string value in return statement\n":  true,
		"# fix\nfix.go:5:2: declared and not used: x\n":                                                 true,
		"fix.go:5:9: mismatched types int and string\n":                                                 true,
		"go build fix: write /tmp/x: no space left on device\n":                                         false,
		"fix.go:5:9: invalid operation\ncompile: write: no space left on device\n":                      false,
		"signal: killed\n": false,
		"go: downloading toolchain: cannot find toolchain go1.99\n": false,
		"": false,
	}

	for out, want := range cases {
		if got := compileFailure(out); got != want {
			t.Errorf("compileFailure(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestReportSurfacesErroredMutantsAndLeavesThemOutOfEfficacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	mutants := []mutant{
		{File: "./p/a.go", Line: 1, Column: 2, Type: "X", Outcome: "KILLED"},
		{File: "./p/a.go", Line: 3, Column: 2, Type: "X", Outcome: "LIVED"},
		{File: "./p/a.go", Line: 5, Column: 2, Type: "X", Outcome: "error: build failed: no space left on device"},
	}

	err := writeReport(path, "./p", mutants, 0)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // the report just written
	if err != nil {
		t.Fatal(err)
	}

	var report gremlinsResult

	err = json.Unmarshal(raw, &report)
	if err != nil {
		t.Fatal(err)
	}

	if report.Errored != 1 || report.Efficacy != 50 || !strings.Contains(string(raw), "a.go:5:2 error: build failed") {
		t.Errorf("errored=%d efficacy=%v report=%s, want 1 errored listed by position and efficacy 1 killed of 2 judged", report.Errored, report.Efficacy, raw)
	}

	book := ledger{Packages: map[string]row{}}

	err = book.record("./p", raw, "abc1234", "2026-10-21")
	if err == nil || !strings.Contains(err.Error(), "errored") {
		t.Errorf("record = %v, want a refusal to turn an errored sweep into a floor", err)
	}
}
