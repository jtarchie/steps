package pipeline

// The ask: step — the pipeline asks a person, and later steps read the answer.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/workspace"
)

// askAnswerFile is where an ask: leaves its answer, under the artifact the step names.
const askAnswerFile = "answer"

// runAskStep parks on a question until a person answers it, writing the answer to <name>/answer and, from a closed list, capturing it as ((.:name)). Never cached: the answer reaches later steps' hashes through the file or the capture.
func runAskStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	asked := merkle.AskContent(step)

	// The key is the question as asked, before anyone answers it: a resume reaches the same step with the same chain and finds the row it was answered on.
	key, err := merkle.HashNode(merkle.NodeKindAsk, asked, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (ask %q): %w", i, step.Ask, err)
	}

	answer, err := agent.AskForStep(ctx, r.st, agent.StepQuestion{
		Pipeline: r.cfg.Name, Job: r.jobName, Name: step.Ask, Question: step.Question,
		Options: step.Options, Default: step.Default, StepKey: key, Wait: approvalTimeout(step.Timeout),
	})
	if err != nil {
		err = fmt.Errorf("step %d (ask %q): %w", i, step.Ask, err)
		recordBlock(ctx, r, merkle.Node{Hash: key, ParentHash: parentHash, Kind: merkle.NodeKindAsk, StepIndex: i, Resource: step.Ask, Content: asked}, nil, err)

		return failedAt(key), err
	}

	err = writeAnswer(ctx, r.bw, step.Ask, answer)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (ask %q): %w", i, step.Ask, err)
	}

	// Revealed: a choice from the pipeline's own list is no secret, and free text is never captured.
	if len(step.Options) > 0 {
		runVarsFrom(ctx).set(step.Ask, capture{value: answer})
	}

	content := map[string]any{"answer": answer}
	for name, value := range asked {
		content[name] = value
	}

	hash, err := merkle.HashNode(merkle.NodeKindAsk, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (ask %q): %w", i, step.Ask, err)
	}

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindAsk, StepIndex: i, Resource: step.Ask, Content: content}
	bestEffort(ctx, "RecordNode", recordSucceeded(context.WithoutCancel(ctx), r, node, nil))

	return ran(hash), nil
}

func writeAnswer(ctx context.Context, bw workspace.BuildWorkspace, name, answer string) error {
	space, err := bw.TaskSpace(ctx, name, nil, []string{name}, nil, nil)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}

	defer workspace.CloseSpace(space, name)

	err = os.WriteFile(filepath.Join(space.Dir(), name, askAnswerFile), []byte(answer), 0o600)
	if err != nil {
		return fmt.Errorf("write the answer: %w", err)
	}

	err = space.Capture(ctx)
	if err != nil {
		return fmt.Errorf("keep the answer: %w", err)
	}

	return nil
}
