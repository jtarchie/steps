package agent

// A pipeline's MCP export (config.MCPExport) served as an MCP server: the
// same tool implementations an agent step runs, built from the same ToolSpec
// grants, so a pin, a timeout or a replacement description means exactly
// what it means to a model. What is different is who calls: an agent
// somewhere else, with no step and no conversation around the call. So a
// custom command gets what a step would have lent it — a directory, a runner —
// made for that one call and gone after it, and config refuses built-ins,
// which have no workspace to work in.

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
	closeOnce sync.Once
}

// NewExportServer builds an export's tools under cfg and serves them. ctx bounds the upstream connections, which live until Close.
func NewExportServer(ctx context.Context, cfg *config.Config, export config.MCPExport, hooks ExportHooks) (*ExportServer, error) {
	err := CheckExportedCommands(export)
	if err != nil {
		return nil, fmt.Errorf("export %q: %w", export.Name, err)
	}

	tools, closers, err := buildAgentTools(ctx, cfg, export.Tools, "")
	if err != nil {
		return nil, fmt.Errorf("export %q: %w", export.Name, err)
	}

	calls := callSpace{export: export, kinds: callKinds(export.Tools)}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: cfg.Name + "/" + export.Name, Version: "v1"}, nil)
	served := &ExportServer{closers: closers}

	for _, decl := range tools.decls.FunctionDeclarations {
		if decl == nil {
			continue
		}

		impl, ok := tools.registry[decl.Name]
		if !ok {
			continue
		}

		server.AddTool(&sdkmcp.Tool{Name: decl.Name, Description: decl.Description, InputSchema: declInputSchema(decl)}, exportHandler(decl.Name, impl, calls, hooks))
		served.Tools = append(served.Tools, decl.Name)
	}

	served.Handler = sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true})

	return served, nil
}

// Close ends the upstream connections.
func (e *ExportServer) Close() error {
	e.closeOnce.Do(func() { closeAll(e.closers) })

	return nil
}

// CheckExportedCommands refuses an export whose command would put a caller's value into the shell as syntax. The caller is not the pipeline's author, so a value that reaches sh -c bare is a command the caller chose.
func CheckExportedCommands(export config.MCPExport) error {
	for _, spec := range export.Tools {
		if spec.Run == "" {
			continue
		}

		for _, hit := range UnquotedToolRefs(spec.Run) {
			return fmt.Errorf("tool %q: %s prints a caller's value into the shell unquoted; pipe it through shellquote", spec.Name, hit)
		}
	}

	return nil
}

type callKind int

const (
	// remoteCall needs nothing of this machine: an MCP grant is answered upstream.
	remoteCall callKind = iota
	// commandCall runs a command in a directory of its own, on the host or in the export's container.
	commandCall
	// agentCall is a sub-agent's conversation, which works in a directory of its own and builds its own runner.
	agentCall
)

func callKinds(specs []config.ToolSpec) map[string]callKind {
	kinds := make(map[string]callKind, len(specs))

	for _, spec := range specs {
		switch {
		case spec.Agent != "":
			kinds[spec.Agent] = agentCall
		case spec.MCP == "" && spec.Run != "":
			kinds[spec.Name] = commandCall
		}
	}

	return kinds
}

// callSpace makes what one call works in: nothing for an upstream call, a directory removed afterwards for the rest, and for a command the runner that places it. Nothing lives past the call, so two callers never share a file.
type callSpace struct {
	export config.MCPExport
	kinds  map[string]callKind
}

func (c callSpace) open(tool string) (toolEnv, func(), error) {
	kind := c.kinds[tool]
	if kind == remoteCall {
		return toolEnv{}, func() {}, nil
	}

	dir, err := os.MkdirTemp("", "steps-export-call-")
	if err != nil {
		return toolEnv{}, nil, fmt.Errorf("%s: %w", tool, err)
	}

	remove := func() { _ = os.RemoveAll(dir) }

	if kind == agentCall {
		return toolEnv{dir: dir}, remove, nil
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{
		Image: c.export.Image, Cwd: dir, User: c.export.User, Network: c.export.Network, Privileged: c.export.Privileged,
		CPUShares: c.export.Limits.CPUShares(), MemoryBytes: c.export.Limits.MemoryBytes(),
	})
	if err != nil {
		remove()

		return toolEnv{}, nil, fmt.Errorf("%s: %w", tool, err)
	}

	runner = runner.WithLabel(c.export.Name + "/" + tool)

	return toolEnv{dir: dir, runner: runner}, func() {
		shell.CloseRunner(runner, tool)
		remove()
	}, nil
}

func exportHandler(name string, impl toolImpl, calls callSpace, hooks ExportHooks) sdkmcp.ToolHandler {
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

		env, done, err := calls.open(name)
		if err != nil {
			return exportError(err), nil
		}

		defer done()

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
