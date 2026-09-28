package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
)

// ApprovalsCmd lists approval: steps waiting for a decision.
//
// A parked approval that nobody is told about is useless in practice, so this
// is the "what is waiting on me?" command. It reads the same rows the audit
// trail is made of.
type ApprovalsCmd struct {
	List    ApprovalsListCmd `cmd:"" default:"withargs"                      help:"list approval: steps waiting for a decision"`
	Approve ApproveCmd       `cmd:"" help:"approve a waiting approval: step"`
	Reject  RejectCmd        `cmd:"" help:"reject a waiting approval: step"`
}

// ApprovalsListCmd is the listing itself, and the group's default: bare
// `steps approvals -p <pipeline>` still answers "what is waiting on me?".
type ApprovalsListCmd struct {
	ReadFlags `embed:""`
}

// Run prints every pending approval.
func (a *ApprovalsListCmd) Run() error {
	if nothingRecorded(a.ReadFlags, "no approvals are waiting") {
		return nil
	}

	st, cleanup, err := openStore(a.ReadFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	pending, err := st.Approvals(context.Background(), true, 0)
	if err != nil {
		return fmt.Errorf("could not list approvals: %w", err)
	}

	if len(pending) == 0 {
		fmt.Println("no approvals are waiting")

		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(writer, "ID\tJOB\tREQUESTED\tMESSAGE")

	for _, approval := range pending {
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\n",
			approval.ID, approval.JobName, approval.RequestedAt, approval.Message)
	}

	err = writer.Flush()
	if err != nil {
		return fmt.Errorf("could not write the approvals table: %w", err)
	}

	return nil
}

// ApproveCmd records a yes.
//
// ⚠️ v1 scope, stated deliberately rather than discovered: anyone who can run
// this command can approve. There is no separate identity system — the
// recorded approver is the OS user, which is an audit record, not an
// authorization check. Someone will ask "can anyone approve?" the day this
// ships, and the answer is yes, on purpose, for now.
type ApproveCmd struct {
	ReadFlags `embed:""`
	ID        int64  `arg:""                                       help:"the approval id, from steps approvals"`
	Reason    string `help:"note to record alongside the decision"`
}

// Run approves the named approval.
func (a *ApproveCmd) Run() error {
	return decideApproval(a.ReadFlags, a.ID, "approved", a.Reason)
}

// RejectCmd records a no.
type RejectCmd struct {
	ReadFlags `embed:""`
	ID        int64  `arg:""                                    help:"the approval id, from steps approvals"`
	Reason    string `help:"why — recorded with the decision"`
}

// Run rejects the named approval.
func (r *RejectCmd) Run() error {
	return decideApproval(r.ReadFlags, r.ID, "rejected", r.Reason)
}

// decideApproval records a decision against a pipeline's store.
func decideApproval(flags ReadFlags, id int64, status, reason string) error {
	st, cleanup, err := openStore(flags)
	if err != nil {
		return err
	}
	defer cleanup()

	err = st.DecideApproval(context.Background(), id, status, currentUser(), reason)
	if err != nil {
		return fmt.Errorf("could not record the decision: %w", err)
	}

	fmt.Printf("%s: approval %d\n", status, id)

	return nil
}

// currentUser is the audit record's "who". It is deliberately not an
// authorization check: it records who ran the command on this host, which is
// what someone reconstructing a decision later needs.
func currentUser() string {
	for _, key := range []string{"STEPS_APPROVER", "USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}

	return "unknown"
}
