package sqlscope_test

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/tools/sqlscope"
)

// unscopedByDesign is every statement in internal/store/sqlite allowed to touch a pipeline-scoped table without naming pipeline_id, keyed by a fragment of it, with the reason that is safe. An entry that excuses nothing fails the test, so the list cannot outlive the code it describes.
var unscopedByDesign = map[string]string{
	"(SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM nodes)":     "freshlyCreated asks whether the FILE is empty, which is a question about every pipeline at once",
	"LEFT JOIN pipeline_revisions v ON v.id = p.current_revision_id": "Reader.Pipelines lists every pipeline in the file; the join is one row per pipeline through its own RESTRICTed revision id",
	"JOIN pipeline_revisions r ON r.id = p.current_revision_id":      "CurrentRevision is scoped by WHERE p.id = ?, the pipelines row itself, and reaches the revision through that row's own foreign key",
	"WHERE content_hash NOT IN (SELECT content_hash FROM nodes)":     "node_content is global, keyed by a hash of the content, and its sweep must stay pipeline-blind or the RESTRICT fires (see pruneNodeContent)",
}

// A query without a pipeline_id predicate is the bug CLAUDE.md names: merkle hashes do not fold in the pipeline, so an unscoped cache read lets one pipeline skip work another one did.
func TestEveryStoreStatementOnAScopedTableNamesThePipeline(t *testing.T) {
	report, err := sqlscope.Check("../../internal/store/sqlite")
	if err != nil {
		t.Fatal(err)
	}

	if report.Tables < 5 || report.Statements < 50 {
		t.Fatalf("read %d scoped tables and %d statements: the source walk has stopped working, and an empty report would pass for a clean one", report.Tables, report.Statements)
	}

	used := map[string]bool{}

	for _, finding := range report.Findings {
		excused := false

		for fragment := range unscopedByDesign {
			if strings.Contains(finding.SQL, fragment) {
				used[fragment], excused = true, true
			}
		}

		if !excused {
			t.Errorf("%s: a statement on %s names no pipeline_id. Scope it, or add it to unscopedByDesign with the reason it is safe:\n%s", finding.Pos, finding.Table, finding.SQL)
		}
	}

	for fragment := range unscopedByDesign {
		if !used[fragment] {
			t.Errorf("unscopedByDesign[%q] excuses nothing any more — delete it", fragment)
		}
	}
}

// The fixture is the permanent form of the sabotage: it holds the checker to catching a plain unscoped statement and one split by concatenation, and to leaving a scoped statement and an unscoped TABLE alone.
func TestCheckFindsExactlyTheUnscopedStatements(t *testing.T) {
	report, err := sqlscope.Check("testdata/unscoped")
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		got = append(got, finding.Table+": "+finding.SQL)
	}

	want := []string{
		"nodes: SELECT COUNT(*) FROM nodes WHERE hash = ?",
		"nodes: DELETE FROM nodes WHERE hash IN ( ? )",
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
