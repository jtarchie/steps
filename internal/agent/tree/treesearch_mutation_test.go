package tree

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// treesearchCannedRunner answers every script with one fixed stdout and exit status.
type treesearchCannedRunner struct {
	shell.Runner
	stdout string
	code   int
}

func (r treesearchCannedRunner) RunCaptureFullLimited(context.Context, string, int, string) (string, string, int, error) {
	return r.stdout, "", r.code, nil
}

// TestFindPathsKeepsWhatAFailingFindPrinted pins find's partial answer: an unreadable directory makes find exit nonzero while still printing everything else it walked, and those paths are the search's to keep.
func TestFindPathsKeepsWhatAFailingFindPrinted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		stdout string
		code   int
		want   []string
	}{
		{"partial", "/w/b\n/w/a\n", 1, []string{"/w/a", "/w/b"}},
		{"failed with nothing", "", 1, nil},
		{"succeeded", "/w/a\n", 0, []string{"/w/a"}},
	}

	for _, tc := range cases {
		tree := Container{Runner: treesearchCannedRunner{stdout: tc.stdout, code: tc.code}, dir: "/w", Lost: &Lost{}}

		got, err := tree.findPaths(t.Context(), "find")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("%s: findPaths = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFilterCandidatesStopsAtTheScanBudget pins the same budget searchWalk keeps: the file that would be one past it is not scanned, and the result says the budget cut it off.
func TestFilterCandidatesStopsAtTheScanBudget(t *testing.T) {
	t.Parallel()

	paths := make([]string, 0, MaxSearchFilesScanned+1)
	for i := range MaxSearchFilesScanned + 1 {
		paths = append(paths, fmt.Sprintf("/w/f%06d", i))
	}

	var result SearchResult

	kept := Container{dir: "/w"}.filterCandidates(&result, "/w", paths, SearchOpts{})

	if len(kept) != MaxSearchFilesScanned || !result.BudgetHit {
		t.Errorf("kept %d files, budgetHit = %v; want %d and true", len(kept), result.BudgetHit, MaxSearchFilesScanned)
	}
}

func TestGrepRefusal(t *testing.T) {
	t.Parallel()

	for _, status := range []int{0, grepMatchedNothing, grepSIGPIPE} {
		err := grepRefusal(fmt.Sprintf("%s%d\n", grepStatusPrefix, status), "x")
		if err != nil {
			t.Errorf("status %d: %v, want it read as an answer", status, err)
		}
	}

	err := grepRefusal("\ngrep: bad regex\n\n"+grepStatusPrefix+"2\n", "x(")
	if err == nil {
		t.Fatal("status 2 read as an answer, want a refusal")
	}

	if !strings.HasSuffix(err.Error(), "(exit 2): grep: bad regex") {
		t.Errorf("refusal = %q, want grep's own diagnostic, and only it", err)
	}
}

// TestBatchPathsFillsEachBatchToTheLimit pins the accounting: each path costs its length plus four (quotes, a separator and the base's slash), a batch may reach the limit exactly, and a path too long for any batch still gets one of its own.
func TestBatchPathsFillsEachBatchToTheLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		rels  []string
		limit int
		want  [][]string
	}{
		{"none", nil, 20, nil},
		{"exactly the limit", []string{"aaaaaa", "bbbbbb"}, 20, [][]string{{"aaaaaa", "bbbbbb"}}},
		{"one over", []string{"aaaaaa", "bbbbbb", "c"}, 20, [][]string{{"aaaaaa", "bbbbbb"}, {"c"}}},
		{"one byte under", []string{"aaaaaa", "bbbbbb"}, 19, [][]string{{"aaaaaa"}, {"bbbbbb"}}},
		{"longer than any batch", []string{"a-very-long-name"}, 5, [][]string{{"a-very-long-name"}}},
	}

	for _, tc := range cases {
		got := batchPaths(tc.rels, tc.limit)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: batchPaths = %q, want %q", tc.name, got, tc.want)
		}
	}
}
