package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestExcludeFlagsSkipOnlyWhatDidNotChange(t *testing.T) {
	all := []string{"internal/store/runs.go", "internal/store/sqlite/runs.go", "internal/store/sqlite/nodes.go"}
	changed := map[string]bool{"internal/store/sqlite/runs.go": true}

	got := excludeFlags("./internal/store", all, changed)
	want := []string{"-E", `(^|/)runs\.go$`, "-E", `(^|/)sqlite/nodes\.go$`}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("flags = %v, want %v", got, want)
	}
}

func TestSumNumstatCountsThePackagesOwnSourceOnly(t *testing.T) {
	numstat := "10\t2\tinternal/store/runs.go\n" +
		"40\t0\tinternal/store/runs_test.go\n" +
		"7\t7\tinternal/store/sqlite/runs.go\n" +
		"-\t-\tinternal/store/blob.bin\n" +
		"3\t0\tinternal/store/README.md\n"

	got := sumNumstat(numstat, "./internal/store")
	if got != 12 {
		t.Errorf("changed = %d, want 12: tests, subpackages and non-Go files are not this package's mutable source", got)
	}
}

func TestRankPutsTheUnknownBeforeTheDrifted(t *testing.T) {
	rows := []staleness{
		{pkg: "./a", changed: 900},
		{pkg: "./b", changed: 5, never: true},
		{pkg: "./c", changed: 30},
		{pkg: "./d", changed: 400, never: true},
	}

	rank(rows)

	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.pkg)
	}

	want := []string{"./d", "./b", "./a", "./c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

const gremlinsReport = `{"test_efficacy": 80.0, "mutants_killed": 8, "mutants_lived": 2, "mutants_not_covered": 3,
 "files": [{"file_name": "a.go", "mutations": [
   {"line": 1, "type": "CONDITIONALS_NEGATION", "status": "KILLED"},
   {"line": 2, "type": "CONDITIONALS_NEGATION", "status": "TIMED OUT"},
   {"line": 3, "type": "CONDITIONALS_BOUNDARY", "status": "LIVED"}]}]}`

func TestRecordWritesWhatWasMeasuredAndSaysItIsUntriaged(t *testing.T) {
	book := ledger{Packages: map[string]row{}}

	err := book.record("./internal/foo", []byte(gremlinsReport), "abc1234", "2026-09-19")
	if err != nil {
		t.Fatal(err)
	}

	got := book.Packages["./internal/foo"]
	if got.SweptAt != "abc1234" || got.Killed != 8 || got.Lived != 2 || got.NotCovered != 3 || got.TimedOut != 1 || got.Efficacy != 80 {
		t.Errorf("row = %+v", got)
	}

	if !strings.Contains(got.Note, "NOT triaged") {
		t.Errorf("note = %q, want a measurement with survivors to say nobody has looked at them", got.Note)
	}
}

func TestRecordKeepsWhatTriageWroteDown(t *testing.T) {
	kept := []equivalent{{Mutant: "a.go | CONDITIONALS_BOUNDARY | if n < 2 {", Why: "n is never 2"}}
	book := ledger{Packages: map[string]row{"./internal/foo": {Efficacy: 70, Note: "swept by hand", Equivalent: kept}}}

	err := book.record("./internal/foo", []byte(gremlinsReport), "abc1234", "2026-09-19")
	if err != nil {
		t.Fatal(err)
	}

	got := book.Packages["./internal/foo"]
	if !reflect.DeepEqual(got.Equivalent, kept) || got.Note != "swept by hand" {
		t.Errorf("row = %+v, want the triage notes a re-measurement did not make left alone", got)
	}
}

// The floor only rises. A lower number is the FINDING, and a tool that wrote it over the old one would turn a regression into the new normal.
func TestRecordRefusesToLowerTheFloor(t *testing.T) {
	before := row{SweptAt: "old", Efficacy: 96.99}
	book := ledger{Packages: map[string]row{"./internal/foo": before}}

	err := book.record("./internal/foo", []byte(gremlinsReport), "abc1234", "2026-09-19")
	if err == nil || !strings.Contains(err.Error(), "96.99") || !strings.Contains(err.Error(), "80") {
		t.Fatalf("error = %v, want a refusal naming both numbers", err)
	}

	if !reflect.DeepEqual(book.Packages["./internal/foo"], before) {
		t.Errorf("row = %+v, want it untouched", book.Packages["./internal/foo"])
	}
}

func TestSummaryIsOneLineAndSilentWhenThereIsNothingToSay(t *testing.T) {
	if got := summary(nil); got != "" {
		t.Errorf("summary of nothing = %q", got)
	}

	if got := summary([]staleness{{pkg: "./a", changed: 0}}); got != "" {
		t.Errorf("summary of a freshly swept package = %q, want silence", got)
	}

	got := summary([]staleness{
		{pkg: "./big", changed: 9000, never: true},
		{pkg: "./small", changed: 10, never: true},
		{pkg: "./drifted", changed: 840, swept: "a1b2c3 2026-09-18"},
		{pkg: "./fresh", changed: 0, swept: "a1b2c3 2026-09-18"},
	})

	for _, want := range []string{"2 packages never swept", "./drifted", "840", "mutation-sweep"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary = %q, want it to mention %q", got, want)
		}
	}

	if strings.Contains(got, "\n") {
		t.Errorf("summary = %q, want one line", got)
	}
}

// A parent package silently re-sweeps its children: ./internal/store alone reports exactly sqlite's mutants. Measured, which is the only reason this exists.
func TestChildPackagesAreExcludedFromTheirParent(t *testing.T) {
	all := []string{"./internal/store", "./internal/store/sqlite", "./internal/store/storetest", "./internal/storefront", "./internal/venue"}

	got := childExcludes("./internal/store", all)
	want := []string{"-E", "^sqlite/", "-E", "^storetest/"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("excludes = %v, want %v", got, want)
	}
}

func TestSweepOrderIsCheapFirstAndSerialPackagesLast(t *testing.T) {
	sizes := map[string]int{"./internal/pipeline": 100, "./internal/config": 9000, "./internal/merkle": 2000, "./internal/web": 50}

	got := sweepOrder([]string{"./internal/pipeline", "./internal/config", "./internal/merkle", "./internal/web"}, sizes)
	want := []string{"./internal/merkle", "./internal/config", "./internal/web", "./internal/pipeline"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v: an interrupted night should leave the most rows, and the hours-long serial packages should not be what it was interrupted in", got, want)
	}
}
