package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

func TestCLIEnvCarriesBuildMetadata(t *testing.T) {
	t.Parallel()

	ctx := shell.WithBuildMetadata(context.Background(), shell.BuildMetadata{RunID: "RUN1"})

	if !slices.Contains(cliEnv(ctx, config.ResolvedInvocation{}), "STEPS_RUN_ID=RUN1") {
		t.Error("cliEnv lacks STEPS_RUN_ID")
	}

	for _, kv := range cliEnv(context.Background(), config.ResolvedInvocation{}) {
		if len(kv) > 6 && kv[:6] == "STEPS_" {
			t.Errorf("a bare context added %q", kv)
		}
	}
}
