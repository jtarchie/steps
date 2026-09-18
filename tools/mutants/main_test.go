package main

import (
	"reflect"
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
