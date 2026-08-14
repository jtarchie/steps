package pipeline

// The approval: step — pause the plan and wait for a person.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
)

const (
	// defaultApprovalTimeout bounds a wait nobody bounded. A day is long
	// enough to survive a weekend evening and short enough that a forgotten
	// approval does not hold a worker forever.
	defaultApprovalTimeout = 24 * time.Hour
	// approvalPollInterval is how often a waiting run re-reads the decision.
	// A person is answering, so seconds are free and a busier loop would only
	// spin the CPU.
	approvalPollInterval = 2 * time.Second
)

// runApprovalStep blocks until someone approves or rejects, or the wait
// expires.
//
// The three outcomes are deliberately different classes:
//
//	approved  the plan continues
//	rejected  a FAILURE — a person said no, which is a decision
//	expired   ABORTED — nobody answered, which is not the same thing
//
// Conflating the last two would make a silent expiry indistinguishable from a
// rejection, and "the deploy was rejected" is a very different thing to read
// in a log from "the deploy was never looked at".
func runApprovalStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	id, err := r.st.RequestApproval(ctx, r.jobName, step.Approval.Message)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (approval): %w", i, err)
	}

	timeout := approvalTimeout(step.Approval.Timeout)

	// Loud, and with the exact command to answer it. A parked approval that
	// nobody is told about is useless in practice, and this is the last line
	// anyone sees before the run stops making progress.
	fmt.Printf("approval %d: %s\n", id, step.Approval.Message)
	fmt.Printf("approval %d: waiting up to %s — steps approve <pipeline> %d  |  steps reject <pipeline> %d\n",
		id, timeout, id, id)
	slog.Warn("job.approval_pending",
		"job", r.jobName, "approval", id, "message", step.Approval.Message, "timeout", timeout.String())

	decision, err := awaitApproval(ctx, r.st, id, timeout)

	content := map[string]any{"approval": step.Approval.Message}

	hash, hashErr := merkle.HashNode(merkle.NodeKindApproval, content, parentHash)
	if hashErr != nil {
		return stepResult{}, fmt.Errorf("step %d (approval): %w", i, hashErr)
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindApproval,
		StepIndex: i, Resource: fmt.Sprintf("approval-%d", id), Content: content,
	}

	status := "succeeded"
	if err != nil {
		status = "failed"
	}

	_ = r.st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), r.jobName, status, approvalRecord(decision), err)

	if err != nil {
		return stepResult{}, err
	}

	fmt.Printf("approval %d: approved by %s\n", id, decision.DecidedBy)

	return ran(hash), nil
}

// awaitApproval polls for the decision until one is made or the wait expires.
func awaitApproval(ctx context.Context, st *store.Store, id int64, timeout time.Duration) (store.Approval, error) {
	deadline := time.Now().Add(timeout)

	for {
		approval, err := st.ApprovalStatus(ctx, id)
		if err != nil {
			return store.Approval{}, fmt.Errorf("approval %d: %w", id, err)
		}

		switch approval.Status {
		case "approved":
			return approval, nil
		case "rejected":
			// A person said no. That is a decision about the work, so it is a
			// FAILURE — on_failure fires, and a to: route can act on it.
			//nolint:wrapcheck // Fail only marks the classification; the error is this package's own
			return approval, outcome.Fail(fmt.Errorf("approval %d rejected by %s: %s",
				id, approval.DecidedBy, reasonOrNone(approval.Reason)))
		}

		if time.Now().After(deadline) {
			_ = st.DecideApproval(context.WithoutCancel(ctx), id, "expired", "", "nobody answered within "+timeout.String())

			// Aborted, not failed: nobody decided anything.
			slog.Warn("job.approval_expired", "approval", id, "timeout", timeout.String())

			return approval, fmt.Errorf("approval %d expired unanswered after %s", id, timeout)
		}

		select {
		case <-ctx.Done():
			return approval, ctx.Err() //nolint:wrapcheck // ctx.Err() is a well-known sentinel
		case <-time.After(approvalPollInterval):
		}
	}
}

func reasonOrNone(reason string) string {
	if reason == "" {
		return "no reason given"
	}

	return reason
}

// approvalRecord is what the store keeps about a decision.
func approvalRecord(approval store.Approval) map[string]any {
	if approval.ID == 0 {
		return nil
	}

	record := map[string]any{"approval": approval.ID, "status": approval.Status}

	if approval.DecidedBy != "" {
		record["decided_by"] = approval.DecidedBy
	}

	if approval.Reason != "" {
		record["reason"] = approval.Reason
	}

	return record
}

// approvalTimeout resolves the wait, falling back to the default.
func approvalTimeout(raw string) time.Duration {
	if raw == "" {
		return defaultApprovalTimeout
	}

	parsed, err := config.ParseTimeout(raw)
	if err != nil || parsed <= 0 {
		return defaultApprovalTimeout
	}

	return parsed
}
