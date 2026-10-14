package scheduler

import (
	"context"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
)

type (
	// ExportServer is one MCP export, served; see pipeline.ExportServer.
	ExportServer = pipeline.ExportServer
	// ExportHooks are what the mounting server decides about each call.
	ExportHooks = pipeline.ExportHooks
)

// ErrRateLimited is a call past an export's rate:.
var ErrRateLimited = pipeline.ErrRateLimited //nolint:gochecknoglobals // a re-export, not state

// ServeExport builds one of target's exports under its current configuration. The caller owns the result and closes it when the configuration moves on.
func (r *LocalRunner) ServeExport(ctx context.Context, target *Pipeline, export config.MCPExport, hooks ExportHooks) (*ExportServer, error) {
	return pipeline.NewExportServer(ctx, target.Config(), export, hooks) //nolint:wrapcheck // the export names itself
}
