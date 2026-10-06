package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only operators inside an e2e-only block are mutated, each with the tests that execute it, and applying one swaps exactly that operator.
func TestMutantsInFindsOnlyTheBlocksOperators(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "x.go")
	source := "package x\n\nfunc f(a, b int) bool {\n\tif a == b {\n\t\treturn a < b && b >= 0\n\t}\n\n\treturn a != b\n}\n"

	err := os.WriteFile(file, []byte(source), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// Lines 4-6: the if and its body, not the final return.
	mutants, err := mutantsIn([]attributedBlock{{Block: file + ":4.2,6.3", Tests: []string{"TestA", "TestB"}}})
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(mutants))
	for _, m := range mutants {
		got = append(got, m.From+">"+m.To)

		if len(m.Tests) != 2 {
			t.Errorf("%s at %d:%d carries tests %v, want the block's", m.From, m.Line, m.Column, m.Tests)
		}
	}

	if want := "==>!= <>>= &&>|| >=><"; strings.Join(got, " ") != want {
		t.Fatalf("mutants = %q, want %q (the != on line 8 is outside the block)", strings.Join(got, " "), want)
	}

	if applied := string(mutants[2].apply([]byte(source))); applied != "package x\n\nfunc f(a, b int) bool {\n\tif a == b {\n\t\treturn a < b || b >= 0\n\t}\n\n\treturn a != b\n}\n" {
		t.Errorf("applying && -> || gave:\n%s", applied)
	}
}
