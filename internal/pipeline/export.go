package pipeline

import "github.com/jtarchie/steps/internal/agent"

// The export server is agent's — it is built from the same tools an agent step runs — and reaches the daemon through here, the one package between them that both may import.
type (
	// ExportServer is one MCP export, served.
	ExportServer = agent.ExportServer
	// ExportHooks are what the mounting server decides about each call.
	ExportHooks = agent.ExportHooks
)

var (
	// NewExportServer builds and serves an export.
	NewExportServer = agent.NewExportServer //nolint:gochecknoglobals // a re-export, not state
	// ErrRateLimited is a call past an export's rate:.
	ErrRateLimited = agent.ErrRateLimited //nolint:gochecknoglobals // a re-export, not state
)
