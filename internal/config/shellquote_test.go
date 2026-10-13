package config

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/frioux/leatherman/pkg/shellquote"
)

// TestShellQuoteMatchesLeatherman holds the port to the function
// internal/template's shellquote calls, so one filter means one thing in
// every template a pipeline writes.
func TestShellQuoteMatchesLeatherman(t *testing.T) {
	t.Parallel()

	for _, word := range shellQuoteCorpus {
		assertShellQuoteParity(t, word)
	}
}

func FuzzShellQuoteMatchesLeatherman(f *testing.F) {
	for _, word := range shellQuoteCorpus {
		f.Add(word)
	}

	f.Fuzz(assertShellQuoteParity)
}

//nolint:gochecknoglobals // shared seed corpus
var shellQuoteCorpus = []string{
	"", "approve", "a b", "it's", "''", "'''", "a''b", "x'''y'z",
	"$(touch pwned)", "`id`", "a;b", "a=b", "--flag=x", "path/to:file@v1.2,3%+^!",
	"tab\there", "new\nline", "\x00", "ünïcode", "-", "*", "~user", "#c",
}

func assertShellQuoteParity(t *testing.T, word string) {
	t.Helper()

	want, wantErr := shellquote.Quote([]string{word})
	got, gotErr := shellQuote(word)

	if (wantErr != nil) != (gotErr != nil) || got != want {
		t.Errorf("shellQuote(%q) = %q, %v; leatherman gives %q, %v", word, got, gotErr, want, wantErr)
	}
}

func TestAcrossRendersShellquote(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcross("j", Step{
		Across: []AcrossVar{{Var: "item", Values: []string{"$(touch x)", "plain"}}},
		Task:   "t",
		Run:    "echo {{ .vars.item | shellquote }}",
	})
	if err != nil {
		t.Fatal(err)
	}

	if cells[0].Run != "echo '$(touch x)'" || cells[1].Run != "echo plain" {
		t.Errorf("rendered %q and %q", cells[0].Run, cells[1].Run)
	}
}

// TestAcrossFileTemplateMayShellquote: a file matrix's templates are parsed at
// load with the same functions the cells render with, or the filter the docs
// recommend for exactly that matrix would be a load error.
func TestAcrossFileTemplateMayShellquote(t *testing.T) {
	t.Parallel()

	loadOK(t, fromFileJob+`
    - var: item
      from_file: findings/items.json
    task: work
    inputs: [findings]
    run: echo {{ .vars.item | shellquote }}
`)
}

func TestAcrossCellCap(t *testing.T) {
	t.Parallel()

	axis := func(name string, n int) AcrossVar {
		values := make([]string, n)
		for i := range values {
			values[i] = strconv.Itoa(i)
		}

		return AcrossVar{Var: name, Values: values}
	}

	_, err := ExpandAcross("j", Step{Task: "t", Run: "true", Across: []AcrossVar{axis("a", 40), axis("b", 25)}})
	if err != nil {
		t.Fatalf("exactly %d cells refused: %v", MaxAcrossCells, err)
	}

	_, err = ExpandAcross("j", Step{Task: "t", Run: "true", Across: []AcrossVar{axis("a", 41), axis("b", 25)}})
	if err == nil || !strings.Contains(err.Error(), "1025 cells, above the limit of 1000") {
		t.Fatalf("error = %v, want the product refused", err)
	}

	// 256^8 is 2^64, which multiplied out wraps an int to exactly zero: the
	// refusal must stop multiplying before that lets the matrix through.
	many := make([]AcrossVar, 0, 8)
	for i := range 8 {
		many = append(many, axis(fmt.Sprintf("v%d", i), 256))
	}

	err = checkCellCount("j", staticAxes(many))
	if err == nil {
		t.Fatal("an overflowing product was accepted")
	}
}
