package agent

// A pipeline's MCP export (config.MCPExport) served as an MCP server: the
// same tool implementations an agent step runs, built from the same ToolSpec
// grants, so a pin, a timeout or a replacement description means exactly
// what it means to a model. What is different is who calls: an agent
// somewhere else, with no step and no conversation around the call, which is
// why config refuses built-ins and custom run: tools in an export before any
// of this is built.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// ExportHooks are what the server that mounts an export decides about each call.
type ExportHooks struct {
	// Before may refuse a call — a rate limit — and the refusal reaches the caller as the tool's error result.
	Before func(ctx context.Context, tool string) error
	// After is told every call that ran: the tool, the NAMES of the arguments it was sent (never their values, which can hold anything a caller pasted), and whether it succeeded.
	After func(ctx context.Context, tool string, argNames []string, ok bool)
}

// ExportServer is one export, built and connected to its upstreams.
type ExportServer struct {
	// Handler serves the MCP streamable HTTP protocol, statelessly: every request is answered from the tools alone.
	Handler http.Handler
	// Tools are the names a caller sees, in the order they are offered.
	Tools []string

	closers   []io.Closer
	dir       string
	closeOnce sync.Once
}

// NewExportServer builds an export's tools under cfg and serves them. ctx bounds the upstream connections, which live until Close.
func NewExportServer(ctx context.Context, cfg *config.Config, export config.MCPExport, hooks ExportHooks) (*ExportServer, error) {
	tools, closers, err := buildAgentTools(ctx, cfg, export.Tools, "")
	if err != nil {
		return nil, fmt.Errorf("export %q: %w", export.Name, err)
	}

	// A sub-agent runs in a directory and spills a large result into one; an export has no step to lend it either, so it gets one of its own for as long as it is served.
	dir, err := os.MkdirTemp("", "steps-export-")
	if err != nil {
		closeAll(closers)

		return nil, fmt.Errorf("export %q: %w", export.Name, err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		closeAll(closers)
		_ = os.RemoveAll(dir)

		return nil, fmt.Errorf("export %q: %w", export.Name, err)
	}

	env := toolEnv{dir: dir, runner: runner, spillDir: dir}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: cfg.Name + "/" + export.Name, Version: "v1"}, nil)
	served := &ExportServer{closers: closers, dir: dir}

	for _, decl := range tools.decls.FunctionDeclarations {
		if decl == nil {
			continue
		}

		impl, ok := tools.registry[decl.Name]
		if !ok {
			continue
		}

		server.AddTool(&sdkmcp.Tool{Name: decl.Name, Description: decl.Description, InputSchema: declInputSchema(decl)}, exportHandler(decl.Name, impl, env, hooks))
		served.Tools = append(served.Tools, decl.Name)
	}

	served.Handler = sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true})

	return served, nil
}

// Close ends the upstream connections and removes the export's directory.
func (e *ExportServer) Close() error {
	e.closeOnce.Do(func() {
		closeAll(e.closers)
		_ = os.RemoveAll(e.dir)
	})

	return nil
}

func exportHandler(name string, impl toolImpl, env toolEnv, hooks ExportHooks) sdkmcp.ToolHandler {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			err := json.Unmarshal(req.Params.Arguments, &args)
			if err != nil {
				return nil, fmt.Errorf("tool %q: arguments are not a JSON object: %w", name, err)
			}
		}

		if hooks.Before != nil {
			err := hooks.Before(ctx, name)
			if err != nil {
				return exportError(err), nil
			}
		}

		result := impl(ctx, args, env)
		ok := requiredCallSucceeded(result)

		if hooks.After != nil {
			names := make([]string, 0, len(args))
			for key := range args {
				names = append(names, key)
			}

			slices.Sort(names)
			hooks.After(ctx, name, names, ok)
		}

		payload, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("tool %q: encoding result: %w", name, err)
		}

		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(payload)}}, IsError: !ok}, nil
	}
}

func exportError(err error) *sdkmcp.CallToolResult {
	payload, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		payload = []byte(`{"error":"refused"}`)
	}

	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(payload)}}, IsError: true}
}

// ErrRateLimited is a call past an export's rate:.
var ErrRateLimited = errors.New("rate limit reached for this export; try again shortly")
