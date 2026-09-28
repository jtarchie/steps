package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// QuestionsCmd is the "what is waiting on me?" command for questions, the way
// ApprovalsCmd is for approvals. It reads the same rows the audit trail is
// made of.
//
// Separate from `steps approvals` because the two park for different reasons
// and are answered differently: an approval takes a yes or a no, a question
// takes a fact — and a listing that mixed them would have to leave out the
// options, which are most of what makes a question one keystroke to answer.
type QuestionsCmd struct {
	List   QuestionsListCmd `cmd:"" default:"withargs"                        help:"list ask_user questions waiting for an answer"`
	Answer AnswerCmd        `cmd:"" help:"answer a waiting ask_user question"`
}

// QuestionsListCmd is the listing itself, and the group's default.
type QuestionsListCmd struct {
	ReadFlags `embed:""`
}

// Run prints every question waiting for an answer.
func (q *QuestionsListCmd) Run() error {
	if nothingRecorded(q.ReadFlags, "no questions are waiting") {
		return nil
	}

	st, cleanup, err := openStore(q.ReadFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	pending, err := st.Questions(context.Background(), true, 0)
	if err != nil {
		return fmt.Errorf("could not list questions: %w", err)
	}

	if len(pending) == 0 {
		fmt.Println("no questions are waiting")

		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(writer, "ID\tJOB\tSTEP\tASKED\tQUESTION")

	for _, question := range pending {
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\n",
			question.ID, question.JobName, question.AgentName, question.AskedAt, question.Question)

		// Under the question rather than in a column of its own: an option
		// list is as long as the answers are, and squeezing it into a cell
		// makes the table unreadable exactly when it matters.
		if len(question.Options) > 0 {
			_, _ = fmt.Fprintf(writer, "\t\t\t\toptions: %s\n", strings.Join(question.Options, " | "))
		}
	}

	err = writer.Flush()
	if err != nil {
		return fmt.Errorf("could not write the questions table: %w", err)
	}

	return nil
}

// AnswerCmd supplies the fact a parked agent step is waiting on.
//
// ⚠️ Same v1 scope as ApproveCmd, stated deliberately: anyone who can run this
// command can answer. The recorded answerer is the OS user, which is an audit
// record and not an authorization check.
type AnswerCmd struct {
	ReadFlags `embed:""`
	ID        int64 `arg:""   help:"the question id, from steps questions"`
	// Variadic so an answer can be written without quoting it, which is what
	// somebody typing a sentence back at a parked step will do.
	Answer []string `arg:"" help:"the answer — one of the offered options, or your own words"`
}

// Run answers the named question.
func (a *AnswerCmd) Run() error {
	st, cleanup, err := openStore(a.ReadFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	answer := strings.TrimSpace(strings.Join(a.Answer, " "))
	if answer == "" {
		return errors.New("an answer is required: steps questions answer -p <pipeline> <id> <answer>")
	}

	err = st.AnswerQuestion(context.Background(), a.ID, answer, currentUser())
	if err != nil {
		return fmt.Errorf("could not record the answer: %w", err)
	}

	fmt.Printf("answered: question %d\n", a.ID)

	return nil
}
