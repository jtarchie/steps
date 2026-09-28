package main

import (
	"slices"
	"testing"
)

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
