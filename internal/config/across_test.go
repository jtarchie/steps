package config

import (
	"strings"
	"testing"
)

// TestAcrossCellOverSharedTaskResolves is the regression: a matrix cell that
// references a reusable tasks: entry by name.
//
// The coordinates used to be appended to cell.Task, which is the field
// ResolveTask looks the shared task up by, so every such matrix died with
// `no task named "shared [shard=b]"`. Inline run: cells never hit it, because
// ResolveTask short-circuits on run: before the lookup — which is exactly why
// this went unnoticed.
func TestAcrossCellOverSharedTaskResolves(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
tasks:
- name: shared
  run: "true"
jobs:
- name: j
  plan:
  - across:
    - var: shard
      values: [a, b]
    task: shared
    inputs: []
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	cells, err := ExpandAcross("job \"j\" step 0", cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, cell := range cells {
		// The lookup key is untouched...
		if cell.Task != "shared" {
			t.Errorf("cell %d task = %q, want %q — the lookup key must not carry coordinates", i, cell.Task, "shared")
		}

		// ...and resolution therefore succeeds, which is the bug.
		_, err := cfg.ResolveTask(cell)
		if err != nil {
			t.Errorf("cell %d: ResolveTask: %v", i, err)
		}
	}

	// ...while the identities are still distinct.
	if a, b := cells[0].DisplayName(), cells[1].DisplayName(); a == b {
		t.Errorf("both cells are named %q; a matrix's cells must be tellable apart", a)
	}
}

// TestAcrossAgentCellsGetDistinctNames is what the split unblocks: an agent
// cell can finally be named, because the name no longer has to double as the
// FindAgent key. Every agent cell of a matrix used to answer to one name,
// which is why context: is rejected on across: steps.
func TestAcrossAgentCellsGetDistinctNames(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcross("job \"j\" step 0", Step{
		Across:   []AcrossVar{{Var: "shard", Values: []string{"a", "b"}}},
		Agent:    "reviewer",
		Messages: []string{"review it"},
	})
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, want := range []string{"reviewer [shard=a]", "reviewer [shard=b]"} {
		if got := cells[i].DisplayName(); got != want {
			t.Errorf("cell %d name = %q, want %q", i, got, want)
		}

		// The agent to invoke is untouched, or FindAgent would fail.
		if cells[i].Agent != "reviewer" {
			t.Errorf("cell %d agent = %q, want reviewer", i, cells[i].Agent)
		}
	}
}

// TestAcrossAuthorNamedCellsAreLeftAlone proves the naming defers to an author
// who distinguished the cells themselves, and that it decides that from the
// TEMPLATE rather than from the rendered text.
//
// The old check looked for a value as a substring of the rendered name, which
// is a coincidence detector: values [a, b] over a task named "shared" found
// the "a" in "shared" and left that cell unsuffixed while its sibling got one.
func TestAcrossAuthorNamedCellsAreLeftAlone(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcross("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "shard", Values: []string{"a", "b"}}},
		Task:   "check-{{ .vars.shard }}",
		Run:    "true",
	})
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, want := range []string{"check-a", "check-b"} {
		if got := cells[i].DisplayName(); got != want {
			t.Errorf("cell %d name = %q, want %q (an author-named cell gets no suffix)", i, got, want)
		}

		if cells[i].Label != "" {
			t.Errorf("cell %d label = %q, want none", i, cells[i].Label)
		}
	}
}

// TestOrdinaryStepHasNoLabel pins the value-gating: a step outside a matrix
// carries no Label, so it hashes exactly as it did before the field existed.
func TestOrdinaryStepHasNoLabel(t *testing.T) {
	t.Parallel()

	step := Step{Task: "build", Run: "true"}
	if step.Label != "" {
		t.Errorf("Label = %q, want empty on an ordinary step", step.Label)
	}

	if got := step.DisplayName(); got != "build" {
		t.Errorf("DisplayName() = %q, want the task name", got)
	}
}

// TestValidateAcrossTemplatesOnRuntimeMatrix covers the one check a runtime
// matrix used to skip entirely (#40.3): its cells cannot expand at load, and
// template validation was a side effect of expanding.
//
// Parse-only, so the object case must still pass — there is no dummy value set
// that satisfies both a string axis and an object axis, which is why nothing is
// rendered here.
// TestAcrossRendersContextPaths covers the per-cell evidence pack: each cell
// arrives already holding the code it was assigned, rather than spending its
// first turns navigating to it.
func TestAcrossRendersContextPaths(t *testing.T) {
	t.Parallel()

	t.Run("static axis", func(t *testing.T) {
		t.Parallel()

		cells, err := ExpandAcross(`job "j" step 0`, Step{
			Across:       []AcrossVar{{Var: "pkg", Values: []string{"agent", "pipeline"}}},
			Agent:        "reviewer",
			Messages:     []string{"review {{ .vars.pkg }}"},
			ContextPaths: []string{"repo/internal/{{ .vars.pkg }}/step.go", "repo/CLAUDE.md"},
		})
		if err != nil {
			t.Fatalf("ExpandAcross: %v", err)
		}

		for i, want := range []string{"agent", "pipeline"} {
			got := cells[i].ContextPaths
			if len(got) != 2 {
				t.Fatalf("cell %d has %d context paths, want 2", i, len(got))
			}

			if got[0] != "repo/internal/"+want+"/step.go" {
				t.Errorf("cell %d context_paths[0] = %q, want the %s path", i, got[0], want)
			}

			// An entry with no template is left exactly as written; only the
			// entries that name a var differ per cell.
			if got[1] != "repo/CLAUDE.md" {
				t.Errorf("cell %d context_paths[1] = %q, want it untouched", i, got[1])
			}
		}
	})
}

// TestAcrossContextPathsDoNotAliasBetweenCells is the regression the slice
// clone in renderCell exists for.
//
// ExpandAcross builds a cell by assigning the step, which copies the struct
// but SHARES the array behind context_paths. Rendering the elements in place
// meant cell 1 consumed the template and every later cell found cell 1's
// already-rendered path — the same aliasing the Try pointer had, and silent
// rather than loud: every cell would read the FIRST cell's file and review it
// under its own name.
func TestAcrossContextPathsDoNotAliasBetweenCells(t *testing.T) {
	t.Parallel()

	step := Step{
		Across:       []AcrossVar{{Var: "pkg", Values: []string{"a", "b", "c"}}},
		Agent:        "reviewer",
		ContextPaths: []string{"repo/{{ .vars.pkg }}.go"},
	}

	cells, err := ExpandAcross(`job "j" step 0`, step)
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, want := range []string{"repo/a.go", "repo/b.go", "repo/c.go"} {
		if got := cells[i].ContextPaths[0]; got != want {
			t.Errorf("cell %d context_paths[0] = %q, want %q", i, got, want)
		}
	}

	// The step the caller passed in is not consumed either: validateAcross
	// expands a static matrix at load and the pipeline expands it again to run
	// it, off the same Step.
	if step.ContextPaths[0] != "repo/{{ .vars.pkg }}.go" {
		t.Errorf("source step context_paths[0] = %q, want the template untouched", step.ContextPaths[0])
	}
}

// TestAcrossContextPathTemplateErrors proves a context path is checked exactly
// like a prompt is — as a side effect of expanding the matrix.
func TestAcrossContextPathTemplateErrors(t *testing.T) {
	t.Parallel()

	_, err := ExpandAcross(`job "j" step 0`, Step{
		Across:       []AcrossVar{{Var: "pkg", Values: []string{"a"}}},
		Agent:        "reviewer",
		ContextPaths: []string{"repo/{{ .vars.pgk }}.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "context_paths[0]") {
		t.Fatalf("error = %v, want it to name the offending path entry", err)
	}
}
