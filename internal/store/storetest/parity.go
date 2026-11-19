package storetest

// Contract tests that once lived in the sqlite driver's package, measured there against raw rows, and so never ran against Postgres.

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestStepBlobsEvictWholeEntriesByCount pins the bound and its shape: count, never age, and whole entries rather than stray rows — so pressure costs a re-run, not a half-answer.
func (s suite) TestStepBlobsEvictWholeEntriesByCount(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	for i := range store.StepBlobEntryCap + 1 {
		err := st.RecordStepBlobs(ctx, fmt.Sprintf("key-%d", i), map[string]string{"out": "d", "logs": "d2"})
		if err != nil {
			t.Fatalf("RecordStepBlobs %d: %v", i, err)
		}
	}

	assertBlobs(t, st, "key-0", map[string]string{})
	assertBlobs(t, st, "key-1", map[string]string{"out": "d", "logs": "d2"})
	assertBlobs(t, st, fmt.Sprintf("key-%d", store.StepBlobEntryCap), map[string]string{"out": "d", "logs": "d2"})
}

// TestRevisionsAreBoundedWhenNoRunIsReaped: an operator iterating on a pipeline with a daemon watching mints a multi-kilobyte row per save, and run_history: 0 (or a prune with no job) reaps no run, which is exactly when the run pass used to return before reaching the configurations.
func (s suite) TestRevisionsAreBoundedWhenNoRunIsReaped(t *testing.T) {
	t.Parallel()

	for name, policy := range map[string]store.Retention{
		"no job":         {},
		"unlimited runs": {JobName: "build", Runs: 0},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ctxFor(t)
			st := s.open(t, "test")

			const edits = 20

			for edit := 1; edit <= edits; edit++ {
				err := st.RecordRevision(ctx, fmt.Sprintf("sha-%03d", edit), pipelineSource(edit), nil)
				if err != nil {
					t.Fatalf("RecordRevision: %v", err)
				}

				err = st.Prune(ctx, policy, "")
				if err != nil {
					t.Fatalf("Prune: %v", err)
				}
			}

			var survivors []string

			for edit := 1; edit <= edits; edit++ {
				sha := fmt.Sprintf("sha-%03d", edit)
				if revisionExists(ctx, t, st, sha) {
					survivors = append(survivors, sha)
				}
			}

			if !slices.Equal(survivors, []string{fmt.Sprintf("sha-%03d", edits)}) {
				t.Errorf("%v survive %d saves that ran nothing, want only the newest", survivors, edits)
			}
		})
	}
}

// TestQuestionsAreReapedWithTheirRun: the table is run-scoped, so a pruned run takes its questions with it and nothing has to remember a second DELETE.
func (s suite) TestQuestionsAreReapedWithTheirRun(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	asked := map[string]int64{}

	for n := 1; n <= 6; n++ {
		runID := fmt.Sprintf("RUN%05d", n)
		build(ctx, t, st, "answer-mention", runID, n)

		question, _ := askBump(t, st, runID)
		asked[runID] = question.ID
	}

	err := st.Prune(ctx, store.Retention{JobName: "answer-mention", Runs: 2}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	all, err := st.Questions(ctx, false, 0)
	if err != nil {
		t.Fatalf("Questions: %v", err)
	}

	runs := make([]string, 0, len(all))
	for _, question := range all {
		runs = append(runs, question.RunID)
	}

	slices.Sort(runs)

	if !slices.Equal(runs, []string{"RUN00005", "RUN00006"}) {
		t.Errorf("questions of %v survived a prune to 2 runs, want those of RUN00005 and RUN00006", runs)
	}

	_, err = st.QuestionStatus(ctx, asked["RUN00001"])
	if err == nil {
		t.Error("a reaped run's question is still readable")
	}
}

// TestRenamingOntoATakenNameIsRefused: two pipelines in one database cannot share a name, and a refused rename must leave both as they were — the pipeline it would have overwritten above all.
func (s suite) TestRenamingOntoATakenNameIsRefused(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	app, infra := s.open(t, "app"), s.open(t, "infra")

	seedRuns(t, app, "build", "app-run")
	seedRuns(t, infra, "build", "infra-run")

	err := app.Rename(ctx, "infra")
	if !errors.Is(err, store.ErrPipelineExists) {
		t.Fatalf("Rename onto a taken name = %v, want store.ErrPipelineExists", err)
	}

	if names := pipelineNames(t, app); !slices.Contains(names, "app") || !slices.Contains(names, "infra") {
		t.Errorf("after a refused rename the database holds %v, want both app and infra", names)
	}

	for st, want := range map[store.Store]string{app: "app-run", infra: "infra-run"} {
		runs, err := st.ListRuns(ctx, "build", 0)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}

		if len(runs) != 1 || runs[0].ID != want {
			t.Errorf("after a refused rename a pipeline reports %+v, want only %s", runs, want)
		}
	}
}
