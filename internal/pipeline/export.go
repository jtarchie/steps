package pipeline

import (
	"context"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
)

// The export server is agent's — it is built from the same tools an agent step runs — and reaches the daemon through here, the one package between them that both may import.
type (
	// ExportServer is one MCP export, served.
	ExportServer = agent.ExportServer
	// ExportHooks are what the mounting server decides about each call.
	ExportHooks = agent.ExportHooks
	// ExportWho is the person a call is made as, as the rules checking it see them.
	ExportWho = agent.ExportWho
)

var (
	// WithExportWho carries who is calling to the rules checking the call.
	WithExportWho = agent.WithExportWho //nolint:gochecknoglobals // a re-export, not state
	// ErrRateLimited is a call past an export's rate:.
	ErrRateLimited = agent.ErrRateLimited //nolint:gochecknoglobals // a re-export, not state
)

// NewExportServer builds and serves an export, its image: calls through the venue so they run on volumes like every other container step.
func NewExportServer(ctx context.Context, cfg *config.Config, export config.MCPExport, hooks ExportHooks) (*ExportServer, error) {
	hooks.Runner = func(_ context.Context, spec shell.RunnerSpec) (shell.Runner, error) {
		return newRunner(venue.Worker{}, spec)
	}

	return agent.NewExportServer(ctx, cfg, export, hooks) //nolint:wrapcheck // the export names itself
}
