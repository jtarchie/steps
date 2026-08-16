package pipeline

// The per-step output cache: asking whether this exact work, over these exact
// input bytes, has already been done.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/workspace"
)

// stepCacheRequest describes a step to the cache: what the work IS, and which
// artifacts it reads and writes.
//
// ContentHash is the step's own content hashed with NO parent, unlike the node
// hash the same content produces a line later. That is deliberate: a node hash
// carries the whole chain that led to the step, so the same work in another
// job — or after an unrelated upstream step re-ran identically — would key
// differently and never hit. What identifies the work is the step's content
// and the bytes it reads, and the bytes are the half the workspace supplies.
func stepCacheRequest(
	kind merkle.NodeKind, content map[string]any,
	inputs, outputs []string, inputMapping, outputMapping map[string]string,
) (workspace.StepCacheRequest, error) {
	contentHash, err := merkle.HashNode(kind, content, "")
	if err != nil {
		return workspace.StepCacheRequest{}, fmt.Errorf("step cache key: %w", err)
	}

	return workspace.StepCacheRequest{
		ContentHash:   contentHash,
		Inputs:        inputs,
		InputMapping:  inputMapping,
		Outputs:       outputs,
		OutputMapping: outputMapping,
	}, nil
}

// taskCacheRequest is stepCacheRequest for a resolved task step.
func taskCacheRequest(content map[string]any, step config.Step, rt config.ResolvedTask) (workspace.StepCacheRequest, error) {
	return stepCacheRequest(
		merkle.NodeKindTask, content,
		rt.Inputs, rt.Outputs,
		rt.InputMapping, config.CollectedOutputMapping(rt.Outputs, rt.OutputMapping, step.OutputSubdir),
	)
}

// lookupStepCache reports the cache entry a step's work is filed under, and
// whether its outputs were already there and have been restored.
//
// A step the cache must never reuse (see merkle.StepCacheable) is not looked
// up at all, and gets the zero result — no key, no hit — which reads at the
// call site as "run it, and file nothing".
func lookupStepCache(
	ctx context.Context, r stepRunner, step config.Step, req workspace.StepCacheRequest, name string,
) workspace.StepCacheResult {
	if !merkle.StepCacheable(r.cfg, step) {
		return workspace.StepCacheResult{}
	}

	res := workspace.LookupStepCache(ctx, r.bw, req)
	if res.Hit {
		// Named "reused" rather than "cached": the chain skip already prints
		// "(cached)", means something different by it, and the two land in the
		// same transcript.
		fmt.Printf("skip: %s (reused)\n", name)
		logFrom(ctx).Info("job.skip", "step", name, "reason", "reused", "key", res.Key)
	}

	return res
}
