package config

// Expansion rules for a from: axis whose items are flat objects (#42): field
// access, cell naming, and the two spellings that have no answer.

import (
	"strings"
	"testing"
)

// TestExpandAcrossValuesSubstitutesObjectFields is the feature: a cell names
// the fields it wants, and each renders as its own text.
func TestExpandAcrossValuesSubstitutesObjectFields(t *testing.T) {
	t.Parallel()

	step := Step{
		Across: []AcrossVar{{Var: "finding", From: "findings", Label: "id"}},
		Agent:  "verifier",
		Prompt: "Confirm {{ .vars.finding.claim }} at {{ .vars.finding.file }}:{{ .vars.finding.line }}",
	}

	cells, err := ExpandAcrossValues("job \"j\" step 0", step, map[string][]any{
		"finding": {
			map[string]string{"id": "SQLI-42", "file": "users.py", "line": "42", "claim": "unparameterized query"},
			map[string]string{"id": "AUTH-7", "file": "api.py", "line": "7", "claim": "missing auth check"},
		},
	})
	if err != nil {
		t.Fatalf("ExpandAcrossValues: %v", err)
	}

	if len(cells) != 2 {
		t.Fatalf("cells = %d, want one per item", len(cells))
	}

	want := []string{
		"Confirm unparameterized query at users.py:42",
		"Confirm missing auth check at api.py:7",
	}

	for i := range want {
		if cells[i].Prompt != want[i] {
			t.Errorf("cell %d prompt = %q, want %q", i, cells[i].Prompt, want[i])
		}
	}

	// label: names the cells. Without it they would all answer to "verifier",
	// which is unroutable and unreadable in a log.
	for i, name := range []string{"verifier [finding=SQLI-42]", "verifier [finding=AUTH-7]"} {
		if cells[i].Label != name {
			t.Errorf("cell %d label = %q, want %q", i, cells[i].Label, name)
		}
	}
}

// TestExpandAcrossValuesNamesUnlabeledObjectCellsByPosition covers the axis
// that declares no label:. A cell still needs an identity distinct from its
// siblings — routing and assert.execution both read it — so it gets its
// position. Deterministic, if uninformative.
func TestExpandAcrossValuesNamesUnlabeledObjectCellsByPosition(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcrossValues("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "item", From: "items"}},
		Task:   "work",
		Run:    "echo {{ .vars.item.name }}",
	}, map[string][]any{
		"item": {
			map[string]string{"name": "first"},
			map[string]string{"name": "second"},
		},
	})
	if err != nil {
		t.Fatalf("ExpandAcrossValues: %v", err)
	}

	for i, name := range []string{"work [item=#1]", "work [item=#2]"} {
		if cells[i].Label != name {
			t.Errorf("cell %d label = %q, want %q", i, cells[i].Label, name)
		}
	}
}

// TestExpandAcrossValuesRejectsBareObjectInterpolation covers the spelling this
// feature deliberately has no answer for. Rendered, it would put Go's own map
// syntax into a command; naming a field is the whole interface.
func TestExpandAcrossValuesRejectsBareObjectInterpolation(t *testing.T) {
	t.Parallel()

	_, err := ExpandAcrossValues("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "finding", From: "findings"}},
		Task:   "work",
		Run:    "echo {{ .vars.finding }}",
	}, map[string][]any{
		"finding": {map[string]string{"id": "a"}},
	})
	if err == nil {
		t.Fatal("a bare object interpolation expanded")
	}

	if !strings.Contains(err.Error(), "name a field") {
		t.Errorf("error does not say what to do instead: %v", err)
	}
}

// TestExpandAcrossValuesRejectsBareObjectInsideABranch closes the hole a
// text-level check would leave: the refusal has to see inside {{ if }},
// {{ range }} and {{ with }} bodies, or a bare object hidden in one still
// renders Go's map syntax into a command.
func TestExpandAcrossValuesRejectsBareObjectInsideABranch(t *testing.T) {
	t.Parallel()

	_, err := ExpandAcrossValues("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "finding", From: "findings"}},
		Task:   "work",
		Run:    `{{ if .vars.finding.id }}echo {{ .vars.finding }}{{ end }}`,
	}, map[string][]any{
		"finding": {map[string]string{"id": "a"}},
	})
	if err == nil {
		t.Fatal("a bare object inside an if body expanded")
	}

	if !strings.Contains(err.Error(), "name a field") {
		t.Errorf("error does not say what to do instead: %v", err)
	}
}

// TestExpandAcrossValuesRejectsAMissingField is the all-or-nothing check that
// makes an unreviewed array safe to fan out over: every cell is rendered before
// any of them runs, so one item missing the field a template names fails the
// whole block rather than cell 7 of 40.
func TestExpandAcrossValuesRejectsAMissingField(t *testing.T) {
	t.Parallel()

	_, err := ExpandAcrossValues("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "finding", From: "findings"}},
		Task:   "work",
		Run:    "echo {{ .vars.finding.file }}",
	}, map[string][]any{
		"finding": {
			map[string]string{"file": "a.py"},
			map[string]string{"claim": "no file field here"},
		},
	})
	if err == nil {
		t.Fatal("an item missing an interpolated field expanded")
	}
}

// TestExpandAcrossValuesRejectsAMissingLabelField covers the same rule for the
// naming field. Falling back to the position for one cell would produce a
// matrix whose names cannot be predicted from the pipeline, which is what
// routing and assert.execution depend on.
func TestExpandAcrossValuesRejectsAMissingLabelField(t *testing.T) {
	t.Parallel()

	_, err := ExpandAcrossValues("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "finding", From: "findings", Label: "id"}},
		Task:   "work",
		Run:    "echo {{ .vars.finding.file }}",
	}, map[string][]any{
		"finding": {
			map[string]string{"id": "A-1", "file": "a.py"},
			map[string]string{"file": "b.py"},
		},
	})
	if err == nil {
		t.Fatal("an item with no label field expanded")
	}

	if !strings.Contains(err.Error(), "label:") {
		t.Errorf("error does not name the label rule: %v", err)
	}
}

// TestAcrossObjectCellsHashLikeTheTextTheyRender is the caching claim, checked
// where it is actually decided: a cell's identity is its RENDERED step, so a
// field no template mentions cannot change what a cell is. Two arrays differing
// only in an unreferenced field must produce byte-identical cells.
func TestAcrossObjectCellsHashLikeTheTextTheyRender(t *testing.T) {
	t.Parallel()

	expand := func(extra string) []Step {
		t.Helper()

		cells, err := ExpandAcrossValues("job \"j\" step 0", Step{
			Across: []AcrossVar{{Var: "finding", From: "findings", Label: "id"}},
			Task:   "work",
			Run:    "echo {{ .vars.finding.file }}",
		}, map[string][]any{
			"finding": {map[string]string{"id": "A-1", "file": "a.py", "noise": extra}},
		})
		if err != nil {
			t.Fatalf("ExpandAcrossValues: %v", err)
		}

		return cells
	}

	first, second := expand("one"), expand("two")

	if first[0].Run != second[0].Run || first[0].Label != second[0].Label {
		t.Errorf("an unreferenced field changed the cell:\n  %+v\n  %+v", first[0], second[0])
	}
}

// TestAcrossLabelOnStaticAxisIsRejected: a values: axis holds strings that
// already name their own cells, so label: there names a field of something with
// no fields.
func TestAcrossLabelOnStaticAxisIsRejected(t *testing.T) {
	t.Parallel()

	err := validateAcrossAxes("job \"j\" step 0", []AcrossVar{
		{Var: "suite", Values: []string{"unit"}, Label: "id"},
	})
	if err == nil {
		t.Fatal("label: on a values: axis validated")
	}

	if !strings.Contains(err.Error(), "already name themselves") {
		t.Errorf("error does not explain why: %v", err)
	}
}
