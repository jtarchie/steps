package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestFilterTestsRefusesAnEmptyList(t *testing.T) {
	_, _, err := filterTests("BenchmarkX\n", "")
	if err == nil {
		t.Fatal("no runnable tests listed: want an error, not a run that checks nothing")
	}

	_, _, err = filterTests("TestA\nTestB\n", "^Test")
	if err == nil {
		t.Fatal("skip expression excluded every test: want an error")
	}
}

func TestFilterTestsCountsWhatSkipExcluded(t *testing.T) {
	names, skipped, err := filterTests("TestA\nTestB\nTestC\nBenchmarkD\n", "^TestB$")
	if err != nil || skipped != 1 || !slices.Equal(names, []string{"TestA", "TestC"}) {
		t.Fatalf("names=%v skipped=%d err=%v, want [TestA TestC] 1 nil", names, skipped, err)
	}
}

func TestRunShardFailsWhenTheBinaryExitsZeroWithoutRunningTests(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake.test")

	err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700) //nolint:gosec // a fake test binary must be executable
	if err != nil {
		t.Fatal(err)
	}

	result := runShard(context.Background(), bin, t.TempDir(), []string{"TestA", "TestB"}, false, time.Minute)
	if result.err == nil {
		t.Fatal("binary exited 0 having run no tests: want the shard to fail")
	}
}

func TestPartitionBalancesByKnownDurationAndRunsEveryTestOnce(t *testing.T) {
	known := map[string]float64{"TestSlow": 10, "TestMid": 6, "TestA": 2, "TestB": 2, "TestC": 2}
	names := []string{"TestA", "TestB", "TestC", "TestMid", "TestSlow", "TestNew"}

	groups := partition(names, known, 2)

	var all []string

	loads := make([]float64, len(groups))

	for i, group := range groups {
		all = append(all, group...)

		for _, name := range group {
			took, ok := known[name]
			if !ok {
				took = 2
			}

			loads[i] += took
		}
	}

	slices.Sort(all)

	want := slices.Clone(names)
	slices.Sort(want)

	if !slices.Equal(all, want) {
		t.Fatalf("tests across shards = %v, want each of %v exactly once", all, want)
	}

	if loads[0] != 12 || loads[1] != 12 {
		t.Errorf("shard loads = %v, want [12 12]: longest first onto the lightest shard, an unknown test costing the median", loads)
	}
}

func TestPartitionNeverMakesAnEmptyShard(t *testing.T) {
	groups := partition([]string{"TestOnly"}, nil, 8)

	if len(groups) != 1 || len(groups[0]) != 1 {
		t.Errorf("groups = %v, want one shard holding the one test", groups)
	}
}
