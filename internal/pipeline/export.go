package pipeline

import "github.com/jtarchie/steps/internal/agent"

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
	// NewExportServer builds and serves an export.
	NewExportServer = agent.NewExportServer //nolint:gochecknoglobals // a re-export, not state
	// WithExportWho carries who is calling to the rules checking the call.
	WithExportWho = agent.WithExportWho //nolint:gochecknoglobals // a re-export, not state
	// ErrRateLimited is a call past an export's rate:.
	ErrRateLimited = agent.ErrRateLimited //nolint:gochecknoglobals // a re-export, not state
)
