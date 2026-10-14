package cli

import (
	"path/filepath"
	"regexp"
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

	assertDecidedBy(t, state, approval, question, "os:release-captain")
}

// A second decision on a row is refused with who got there first, not as a
// broken database: the person needs to know their "reject" did not land on an
// approval somebody else already allowed; not parallel, it sets the environment.
func TestASecondDecisionSaysWhoAlreadyDecided(t *testing.T) {
	t.Setenv("STEPS_APPROVER", "release-captain")

	state := filepath.Join(t.TempDir(), "steps.db")
	approval, _ := pendingDecisions(t, state)
	id := strconv.FormatInt(approval, 10)

	captureStdout(t, func() {
		err := Run([]string{"approvals", "approve", id, "-p", "app", "--db", state})
		if err != nil {
			t.Fatalf("first decision: %v", err)
		}
	})

	t.Setenv("STEPS_APPROVER", "late-comer")

	err := Run([]string{"approvals", "reject", id, "-p", "app", "--db", state})
	if err == nil {
		t.Fatal("a second decision succeeded")
	}

	if !strings.Contains(err.Error(), "already approved by release-captain") || strings.Contains(err.Error(), "could not record") {
		t.Errorf("second decision = %q; want it to name the decision already recorded, not a failure", err)
	}
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

	err = st.StartRun(t.Context(), store.NewRun{ID: "RUN", JobName: "build", Workspace: t.TempDir()})
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

// The listings print a row's time as the state database stores it — whole-second RFC3339 for approvals and the breaker, a fixed nine-digit fraction for questions and memories — which is what they printed while rows carried the stored text itself; not parallel, it captures stdout.
func TestTheListingsPrintStampsAsStored(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	pendingDecisions(t, state)

	st, err := sqlite.OpenStore(state, "app")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	_, _, err = st.RecordJobOutcome(t.Context(), "build", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	_, _, err = st.Remember(t.Context(), store.Memory{Scope: "U1", Text: "likes go", RunID: "RUN"}, 0)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	const (
		seconds = `\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`
		nano    = `\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{9}Z`
	)

	for command, want := range map[string]string{
		"approvals":         `(?m)^1\s+build\s+` + seconds + `\s+ship it\?$`,
		"questions":         `(?m)^1\s+build\s+fixer\s+` + nano + `\s+which region\?$`,
		"jobs":              `(?m)^build\s+1\s+` + seconds + `\s*$`,
		"memory":            `(?m)^U1\s+1\s+` + nano + `\s*$`,
		"memory --scope U1": `(?m)^1\s+` + nano + `\s+RUN\s+"likes go"$`,
	} {
		var err error

		out := captureStdout(t, func() { err = Run(append(strings.Fields(command), "-p", "app", "--db", state)) })
		if err != nil || !regexp.MustCompile(want).MatchString(out) {
			t.Errorf("steps %s = %v, printing:\n%s\nwant a line matching %s", command, err, out, want)
		}
	}
}
