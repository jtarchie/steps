package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// An approval's message, an agent's question and a run's error can all carry text an untrusted PR chose, and a listing prints it onto the operator's terminal; not parallel, it captures stdout.
func TestListingsDropAnOSC(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordUntrusted(t, state, "\x1b]0;pwned\x07\x1b]8;;http://evil\x1b\\")

	for command, said := range map[string]string{
		"approvals":  "ship it?",
		"questions":  "which region?",
		"runs queue": "boom",
	} {
		var err error

		out := captureStdout(t, func() { err = Run(append(strings.Fields(command), "-p", "app", "--db", state)) })
		if err != nil || !strings.Contains(out, said) {
			t.Errorf("steps %s = %v, printing %q; want %q", command, err, out, said)
		}

		if strings.Contains(out, "\x1b") || strings.Contains(out, "pwned") {
			t.Errorf("steps %s printed an escape sequence: %q", command, out)
		}
	}
}

func recordUntrusted(t *testing.T, state, osc string) {
	t.Helper()

	st, err := sqlite.OpenStore(state, "app")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	_, err = st.RequestApproval(t.Context(), "build", "ship"+osc+" it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	err = st.StartRun(t.Context(), store.NewRun{ID: "RUN", JobName: "build", Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, _, err = st.AskQuestion(t.Context(), store.Question{
		RunID: "RUN", JobName: "build", AgentName: "fixer", Question: "which" + osc + " region?", Options: []string{"us" + osc},
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	err = st.EnqueueJob(t.Context(), "build", "manual")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	claim, _, err := st.ClaimNextJob(t.Context())
	id := claim.ID
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	err = st.CompleteJob(t.Context(), id, "failed", errors.New("boom"+osc))
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	_ = st.Close()
}
