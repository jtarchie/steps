package sqlite

// The question rows a caller of the contract cannot see: what a refused write must not have written.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestAskQuestionRefusesARunFromAnotherPipeline: the run_id foreign key alone
// does not scope this table. In a shared state file a sibling pipeline's run
// satisfies it, and the row would be one this Store could never read back,
// since every read joins runs on pipeline_id.
func TestAskQuestionRefusesARunFromAnotherPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	mine := mustOpenStore(t, path)
	defer func() { _ = mine.Close() }()

	theirs, err := OpenStore(path, "other")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	defer func() { _ = theirs.Close() }()

	err = theirs.StartRun(ctx, store.NewRun{ID: "their-run", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, _, err = mine.AskQuestion(ctx, store.Question{
		RunID: "their-run", JobName: "build", AgentName: "writer", Question: "Which bump?",
	})
	if err == nil {
		t.Fatal("a question was recorded against another pipeline's run")
	}

	var count int

	err = mine.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM questions`).Scan(&count)
	if err != nil {
		t.Fatalf("count questions: %v", err)
	}

	if count != 0 {
		t.Errorf("%d unreachable question row(s) were written", count)
	}
}
