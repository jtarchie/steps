package pipeline

// Delivering a demanded upstream decision to a TASK reader.
//
// An agent reader gets its senders as synthetic tool results, in the
// conversation (see internal/agent's upstreamBlocks). A shell command has no
// conversation, so it gets files instead — one per demanded sender.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
)

// deliverUpstreamFiles writes one file per demanded sender into dir, named for
// the step whose decision it carries.
//
// A sender that has not run yet is skipped rather than written empty: a task
// testing for the file learns "no decision yet" from its absence, which is the
// same thing an agent reader learns from an absent block. Writing an empty
// file would make "decided nothing" and "has not run" the same state.
func deliverUpstreamFiles(ctx context.Context, dir string, step config.Step) error {
	from := step.ContextFrom()
	if len(from) == 0 {
		return nil
	}

	for _, sender := range step.FromSenders() {
		up, found := agent.LookupOutcome(ctx, sender)
		if !found {
			continue
		}

		rendered := agent.RenderUpstream(sender, from[sender], up)
		if rendered == "" {
			continue
		}

		path := filepath.Join(dir, config.UpstreamPath(sender))

		err := os.MkdirAll(filepath.Dir(path), 0o750)
		if err != nil {
			return fmt.Errorf("could not create %s: %w", config.UpstreamDir, err)
		}

		err = os.WriteFile(path, []byte(rendered), 0o600)
		if err != nil {
			return fmt.Errorf("could not deliver %q: %w", sender, err)
		}
	}

	return nil
}
