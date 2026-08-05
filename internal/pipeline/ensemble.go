package pipeline

// The ensemble: step — ask several agents the same question, combine their
// answers into one decision, route on it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// memberVote is one ensemble member's answer.
type memberVote struct {
	agent   string
	verdict string
	note    string
	err     error
}

// runEnsembleStep runs every member concurrently and decides one verdict from
// their votes.
//
// A single model has blind spots: one reviewer's "approve" carries no signal
// about how much to trust it. The cost is the obvious tradeoff — N agents cost
// N times one — which is why this is the step a job budget: is worth setting.
func runEnsembleStep(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, parentHash string, handoff *agent.Handoff,
) (string, stepDisposition, nonGetOutcome, error) {
	content, err := merkle.EnsembleNodeContent(cfg, step)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (ensemble): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindEnsemble, content, parentHash)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (ensemble): %w", i, err)
	}

	fmt.Printf("ensemble: %d agents, decide %s\n", len(step.Ensemble.Agents), step.Ensemble.Decide)
	slog.Debug("job.step", "job", jobName, "index", i, "kind", "ensemble", "members", len(step.Ensemble.Agents))

	votes := runEnsembleMembers(ctx, cfg, jobName, i, step, bw, st, hash, handoff)

	verdict, err := decideEnsemble(ctx, cfg, jobName, i, step, bw, st, hash, votes)

	status := "succeeded"
	if err != nil {
		status = "failed"
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindEnsemble,
		StepIndex: i, Resource: executedStepName(step), Content: content,
	}
	_ = st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), jobName, status, ensembleRecord(votes, verdict), err)

	if err != nil {
		return "", stepRan, nonGetOutcome{}, err
	}

	fmt.Printf("ensemble decide: %s → %s\n", step.Ensemble.Decide, verdict)

	return hash, stepRan, nonGetOutcome{verdict: verdict}, nil
}

// runEnsembleMembers runs every member concurrently and collects its vote.
//
// Each member is an ordinary agent step chained under the ensemble node, so it
// records its own node with its own content hash — which is what makes members
// independently cacheable: editing one member's prompt changes only that
// member's hash.
func runEnsembleMembers(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, blockHash string, handoff *agent.Handoff,
) []memberVote {
	members := step.Ensemble.Agents
	votes := make([]memberVote, len(members))
	logs := make([]*execLog, len(members))

	var wg sync.WaitGroup

	for index := range members {
		wg.Add(1)

		go func() {
			defer wg.Done()

			// The vocabulary is declared once on the block and applied to
			// every member, so the members cannot disagree about what they are
			// voting on.
			member := members[index]
			member.Verdicts = step.Ensemble.EnsembleVerdictsFor()

			votes[index] = memberVote{agent: member.Agent}

			runCtx, memberLog := forkExecLog(ctx)
			logs[index] = memberLog

			_, _, out, err := runNonGetStep(runCtx, cfg, jobName, i, member, bw, st, nil, blockHash, handoff)
			votes[index].verdict, votes[index].note, votes[index].err = out.verdict, out.note, err
		}()
	}

	wg.Wait()

	for _, memberLog := range logs {
		if memberLog != nil {
			mergeExecLog(ctx, memberLog)
		}
	}

	return votes
}

// decideEnsemble turns the members' votes into one verdict.
func decideEnsemble(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, blockHash string, votes []memberVote,
) (string, error) {
	counted, err := countableVotes(step.Ensemble, votes)
	if err != nil {
		return "", err
	}

	if judge := step.Ensemble.JudgeAgent(); judge != "" {
		return runEnsembleJudge(ctx, cfg, jobName, i, step, bw, st, blockHash, counted)
	}

	return applyDecisionRule(step.Ensemble, counted)
}

// countableVotes separates members that voted from members that failed, and
// applies the block's policy for the latter.
//
// A member that ERRORS is not a member that voted. Counting a model failure as
// a vote either way is how a three-agent ensemble silently becomes a
// two-agent one with a different meaning, so the default is to fail and the
// alternative has to be asked for.
func countableVotes(ensemble *config.Ensemble, votes []memberVote) ([]memberVote, error) {
	var (
		counted  []memberVote
		failures []error
	)

	for _, vote := range votes {
		switch {
		case vote.err != nil:
			failures = append(failures, fmt.Errorf("member %q: %w", vote.agent, vote.err))
		case vote.verdict == "":
			failures = append(failures, fmt.Errorf("member %q returned no verdict", vote.agent))
		default:
			counted = append(counted, vote)
		}
	}

	if len(failures) > 0 && ensemble.FailsOnMemberError() {
		return nil, fmt.Errorf("ensemble: %d of %d members did not vote (member_errors: %s): %w",
			len(failures), len(votes), config.MemberErrorsFail, errors.Join(failures...))
	}

	if len(counted) == 0 {
		return nil, fmt.Errorf("ensemble: no member returned a verdict: %w", errors.Join(failures...))
	}

	return counted, nil
}

// applyDecisionRule resolves majority/unanimous/any.
//
// A tie is an error, never a silent pick. With an even membership, or three
// different verdicts and no majority, choosing the first would be an invisible
// bug — the author has to name a policy (a judge agent) rather than inherit
// undefined behavior.
func applyDecisionRule(ensemble *config.Ensemble, votes []memberVote) (string, error) {
	tally := map[string]int{}
	for _, vote := range votes {
		tally[vote.verdict]++
	}

	switch ensemble.Decide {
	case config.DecideUnanimous:
		if len(tally) != 1 {
			return "", fmt.Errorf("ensemble: decide: unanimous, but the members disagreed: %s", renderTally(tally))
		}

		return votes[0].verdict, nil

	case config.DecideAny:
		// verdicts: is ordered, so "any" means the first listed verdict
		// anybody chose — the "one objection is enough" shape.
		for _, verdict := range ensemble.Verdicts {
			if tally[verdict] > 0 {
				return verdict, nil
			}
		}

		return "", fmt.Errorf("ensemble: no member voted for any declared verdict: %s", renderTally(tally))

	default: // majority
		return majorityVerdict(tally, len(votes))
	}
}

// majorityVerdict returns the verdict more than half the voters chose.
func majorityVerdict(tally map[string]int, voters int) (string, error) {
	for verdict, count := range tally {
		if count*2 > voters {
			return verdict, nil
		}
	}

	return "", fmt.Errorf("ensemble: decide: majority, but no verdict has one (%s of %d voters); name an agent in decide: to break ties",
		renderTally(tally), voters)
}

// renderTally formats a vote count deterministically for a message.
func renderTally(tally map[string]int) string {
	parts := make([]string, 0, len(tally))
	for verdict, count := range tally {
		parts = append(parts, fmt.Sprintf("%s=%d", verdict, count))
	}

	sort.Strings(parts)

	return strings.Join(parts, " ")
}

// runEnsembleJudge asks a named agent to decide, giving it the members' votes
// and notes.
//
// The judge is an ordinary agent step, which is the point: its reasoning is
// recorded, inspectable and cached exactly like any other agent run, rather
// than being a black box one level deeper than a single agent already was.
func runEnsembleJudge(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, blockHash string, votes []memberVote,
) (string, error) {
	judgeStep := config.Step{
		Agent:    step.Ensemble.JudgeAgent(),
		Prompt:   judgePrompt(step.Ensemble, votes),
		Verdicts: step.Ensemble.EnsembleVerdictsFor(),
	}

	_, _, out, err := runNonGetStep(ctx, cfg, jobName, i, judgeStep, bw, st, nil, blockHash, nil)
	if err != nil {
		return "", fmt.Errorf("ensemble judge %q: %w", judgeStep.Agent, err)
	}

	if out.verdict == "" {
		return "", fmt.Errorf("ensemble judge %q returned no verdict", judgeStep.Agent)
	}

	return out.verdict, nil
}

// judgePrompt renders what the members said, in declaration order.
func judgePrompt(ensemble *config.Ensemble, votes []memberVote) string {
	var out strings.Builder

	out.WriteString("Several agents were asked the same question and voted independently. ")
	out.WriteString("Decide the final verdict, using their reasoning as evidence rather than as a tally.\n\n")

	for _, vote := range votes {
		fmt.Fprintf(&out, "- %s voted %q", vote.agent, vote.verdict)

		if vote.note != "" {
			fmt.Fprintf(&out, ": %s", vote.note)
		}

		out.WriteString("\n")
	}

	fmt.Fprintf(&out, "\nThe verdicts available to you are: %s.\n", strings.Join(ensemble.Verdicts, ", "))

	return out.String()
}

// ensembleRecord is what the store keeps about a decision: every member's
// vote, and the verdict that came out. Without it a run's record would say
// what was decided but not what it was decided from.
func ensembleRecord(votes []memberVote, verdict string) map[string]any {
	members := make([]any, 0, len(votes))

	for _, vote := range votes {
		member := map[string]any{"agent": vote.agent}

		switch {
		case vote.err != nil:
			member["error"] = vote.err.Error()
		default:
			member["verdict"] = vote.verdict
			if vote.note != "" {
				member["note"] = vote.note
			}
		}

		members = append(members, member)
	}

	record := map[string]any{"members": members}
	if verdict != "" {
		record["verdict"] = verdict
	}

	return record
}
