package mcp

import (
	"context"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
)

// ListServerTools connects to srv, lists its tools, and closes the
// connection — the discovery path `steps mcp tools <pipeline> <server>`
// calls, and a smoke test of connectivity/auth for any auth type.
func ListServerTools(ctx context.Context, srv config.MCPServer) ([]*sdkmcp.Tool, error) {
	client, err := Connect(ctx, srv)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	tools, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", srv.Name, err)
	}

	return tools, nil
}
