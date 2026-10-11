package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A throwaway module whose one docker-shaped test skips without a daemon: the boundary mutant no test pins must live, the negation must die, an uncalled function must be NOT COVERED, and the negative only the docker-shaped test sees must be killed by it in the serial tier — the seam where a classification bug would score it NOT COVERED or LIVED.
func TestSweepJudgesEachMutantByTheTestsThatRunIt(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"go.mod": "module fixture\n\ngo 1.25\n",
		"fix.go": "package fix\n\nfunc Max(a, b int) int {\n\tif a > b {\n\t\treturn a\n\t}\n\n\treturn b\n}\n\nfunc Neg(x int) int { return -x }\n\nfunc Unused(x int) bool { return x == 1 }\n",
		"fix_test.go": "package fix\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func TestMax(t *testing.T) {\n\tif Max(2, 1) != 2 || Max(1, 2) != 2 {\n\t\tt.Fatal(\"max\")\n\t}\n}\n\n" +
			"func TestNeg(t *testing.T) {\n\tif os.Getenv(\"DOCKER_HOST\") == \"unix:///nonexistent.sock\" {\n\t\tt.Skip(\"wants docker\")\n\t}\n\n\tif Neg(3) != -3 {\n\t\tt.Fatal(\"neg\")\n\t}\n}\n",
	}

	for name, body := range files {
		err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(dir)

	path, err := sweepPackage(t.Context(), "./", 2, "")
	if err != nil {
		t.Fatal(err)
	}

	got, killers := readStatuses(t, path)

	if k := killers["INVERT_NEGATIVES@11"]; len(k) != 1 || k[0] != "TestNeg" {
		t.Errorf("the negative was killed by %v, want TestNeg, the docker-shaped test", k)
	}

	want := map[string]string{
		"CONDITIONALS_BOUNDARY@4":  "LIVED",
		"CONDITIONALS_NEGATION@4":  "KILLED",
		"INVERT_NEGATIVES@11":      "KILLED",
		"ARITHMETIC_BASE@11":       "KILLED",
		"CONDITIONALS_NEGATION@13": "NOT COVERED",
	}

	for key, status := range want {
		if got[key] != status {
			t.Errorf("%s = %q, want %q (all: %v)", key, got[key], status, got)
		}
	}
}

// readStatuses is the report's status and killers per "TYPE@line".
func readStatuses(t *testing.T, path string) (map[string]string, map[string][]string) {
	t.Helper()

	raw, err := os.ReadFile(path) //nolint:gosec // the report just written
	if err != nil {
		t.Fatal(err)
	}

	var report struct {
		Files []struct {
			Mutations []struct {
				Type    string   `json:"type"`
				Status  string   `json:"status"`
				Line    int      `json:"line"`
				Killers []string `json:"killed_by"`
			} `json:"mutations"`
		} `json:"files"`
	}

	err = json.Unmarshal(raw, &report)
	if err != nil {
		t.Fatal(err)
	}

	statuses, killers := map[string]string{}, map[string][]string{}

	for _, file := range report.Files {
		for _, m := range file.Mutations {
			key := fmt.Sprintf("%s@%d", m.Type, m.Line)
			statuses[key], killers[key] = m.Status, m.Killers
		}
	}

	return statuses, killers
}
