package cli

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A decision and an answer are audit records, so the command must land the row AND say so, under the person STEPS_APPROVER names ahead of the login; not parallel, it sets the environment and captures stdout.
func TestADecisionAndAnAnswerAreRecordedUnderWhoMadeThem(t *testing.T) {
	t.Setenv("STEPS_APPROVER", "release-captain")

	state := filepath.Join(t.TempDir(), "steps.db")
	approval, question := pendingDecisions(t, state)

	for command, said := range map[string]string{
		"approvals approve " + strconv.FormatInt(approval, 10):               "approved: approval ",
		"questions answer " + strconv.FormatInt(question, 10) + " us-east-2": "answered: question ",
	} {
		var err error

		out := captureStdout(t, func() { err = Run(append(strings.Fields(command), "-p", "app", "--db", state)) })
		if err != nil || !strings.Contains(out, said) {
			t.Errorf("steps %s = %v, printing %q; want it recorded and said", command, err, out)
		}
	}

	assertDecidedBy(t, state, approval, question, "release-captain")
}

func assertDecidedBy(t *testing.T, state string, approval, question int64, who string) {
	t.Helper()

	reopened, err := sqlite.OpenStore(state, "app")
	if err != nil {
		t.Fatalf("reopen state store: %v", err)
	}

	defer func() { _ = reopened.Close() }()

	decided, err := reopened.ApprovalStatus(t.Context(), approval)
	if err != nil || decided.Status != "approved" || decided.DecidedBy != who {
		t.Errorf("approval = %+v, %v; want approved by %s", decided, err, who)
	}

	answered, err := reopened.QuestionStatus(t.Context(), question)
	if err != nil || answered.Answer != "us-east-2" || answered.AnsweredBy != who {
		t.Errorf("question = %+v, %v; want us-east-2 answered by %s", answered, err, who)
	}
}

func pendingDecisions(t *testing.T, state string) (int64, int64) {
	t.Helper()

	st, err := sqlite.OpenStore(state, "app")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	approval, err := st.RequestApproval(t.Context(), "build", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	err = st.StartRun(t.Context(), "RUN", "build", t.TempDir(), "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	question, _, err := st.AskQuestion(t.Context(), store.Question{RunID: "RUN", JobName: "build", AgentName: "fixer", Question: "which region?"})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	return approval, question.ID
}
