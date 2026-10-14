package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/usage"
)

// An agent step's own lines fire mid-build, so they carry the run's identity
// like every other line under it (#121). Not t.Parallel(): swaps the default logger.
func TestAgentLinesCarryTheBuildIdentity(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(events.LogHandler(slog.NewTextHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := events.WithLogAttrs(t.Context(), "pipeline", "app", "job", "review")

	logStepUsage(ctx, usage.Step{Step: "a", Total: 3}, 0)
	logCompactionBudget(ctx, config.ResolvedInvocation{AgentName: "a", CompactAfterTokens: 10})

	for _, msg := range []string{"agent.usage", "agent.compaction_budget"} {
		found := false

		for line := range strings.SplitSeq(buf.String(), "\n") {
			if strings.Contains(line, "msg="+msg+" ") {
				found = true

				if !strings.Contains(line, "pipeline=app") || !strings.Contains(line, "job=review") {
					t.Errorf("%s lacks its identity: %q", msg, line)
				}
			}
		}

		if !found {
			t.Errorf("no %s line:\n%s", msg, buf.String())
		}
	}
}
