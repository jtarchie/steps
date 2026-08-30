package agent

// The bridge that lets a CLI-backed agent step (see cli.go) use the tools its
// pipeline granted it.
//
// A CLI owns its own tool loop, so steps cannot hand it a tool registry the
// way it hands one to a model. What a CLI does accept is an MCP server. So
// the parent process becomes one: every tool the step's grant produced that
// the CLI has no native equivalent for — custom run: tools, mcp_servers:
// grants, and the synthesized verdict/context tools — is re-exported over a
// loopback MCP server the child connects back to.
//
// Two things fall out of this that are worth stating plainly. First, the tool
// implementations are the SAME ones an HTTP agent runs: path confinement,
// output caps and spilling, MCP subsetting and auth all apply unchanged,
// because nothing was reimplemented. Second, a verdict call lands in the
// parent's memory as it happens, over a channel the model cannot forge — the
// CLI never has to be trusted to report what it decided, which is what keeps
// verdict routing meaningful across the process boundary.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// cliBridgeServerName is the MCP server name the child CLI knows the bridge
// by. It prefixes every bridged tool as `mcp__steps__<tool>` in the CLI's own
// namespace, which is also how cliArgs must spell them on --allowedTools.
const cliBridgeServerName = "steps"

// cliBridgeShutdownTimeout bounds how long Close waits for in-flight bridge
// requests. The child is already gone by then; this only covers a tool call
// that outlived it.
const cliBridgeShutdownTimeout = 5 * time.Second

// cliBridge is a running loopback MCP server exposing one step's non-native
// tools, plus what it observed the child do with them.
type cliBridge struct {
	server    *http.Server
	listener  net.Listener
	url       string
	closeOnce sync.Once
	// token authenticates the child. Loopback is not a permission boundary:
	// every process on the host can reach an open localhost port, and what
	// this one serves is the step's custom run: tools (arbitrary shell in the
	// workspace) and its verdict tool (which decides where the job goes next).
	// The child learns the token from the mcp-config file, which is written
	// 0600.
	token string

	// budgets bounds how many times a bridged tool may be called during this
	// attempt; a tool absent from it is unlimited. See counts.
	budgets map[string]int

	// mu guards everything below: tool calls arrive on the HTTP server's
	// goroutines, and the driver reads the captures after the child exits.
	mu        sync.Mutex
	verdict   string
	note      string
	satisfied map[string]bool
	// calls records every bridged tool call in order, so the step's
	// trajectory includes tools the CLI's own stream reports only by their
	// prefixed name.
	calls []recordedToolCall
	// counts is how many times each budgeted tool has been called. The
	// counter lives HERE because this is the only place on the CLI path that
	// sees every call: internal/agent's turn loop enforces max_calls: and a
	// CLI agent does not run one, which is exactly why the load-time guard
	// refuses that field for a cli source. max_questions: is the one budget
	// that must still bind — it is denominated in a person's attention, not
	// in tool calls — so it is enforced here instead of promised and dropped.
	counts map[string]int
}

// bridgeReach says where the child will dial this bridge FROM, which decides
// both what address to bind and what address to advertise. Getting it wrong
// means every bridged tool call — including the verdict — is refused.
type bridgeReach int

const (
	// reachHost: the child is a subprocess on this machine. Loopback, which
	// is reachable by it and by nothing else on the network.
	reachHost bridgeReach = iota
	// reachGateway: the child is in a container, and reaches us by the
	// docker gateway rather than this machine's loopback. Requires binding
	// all interfaces.
	//
	// This covers `network: host` too, which looks like it should be the
	// loopback case and is not: when the daemon runs in a VM (Docker
	// Desktop, colima) that "host" is the VM, so a bridge on this machine's
	// loopback is unreachable from the container — verified against colima,
	// where host.docker.internal resolves under host networking and 127.0.0.1
	// does not answer. A daemon sharing this kernel would make loopback work,
	// but steps cannot tell the two apart from here, and the gateway route
	// is correct for both.
	reachGateway
)

// cliBridgeReach classifies a step's child by the runtime it resolved.
func cliBridgeReach(ri config.ResolvedInvocation) bridgeReach {
	if ri.Image == "" {
		return reachHost
	}

	return reachGateway
}

// newCLIBridge starts a bridge serving every tool in conv's registry except
// those named in skip — the built-ins the CLI runs natively (see
// cliRuntime.natives). The caller must Close it.
//
// reach changes only where the bridge is reachable FROM; see bridgeReach.
// The bearer token is unchanged and is what actually authorizes a request —
// widening the bind widens who may DIAL the port, not what they may do with
// it, and only for the length of one attempt.
//
// The conversation's per-tool call ceilings come along too, because this is
// the only place on the CLI path that sees every call — see cliBridge.counts.
func newCLIBridge(ctx context.Context, conv agentConversation, skip map[string]bool, reach bridgeReach) (*cliBridge, error) {
	bridge := &cliBridge{
		satisfied: map[string]bool{},
		token:     rand.Text(),
		budgets:   conv.tools.maxCalls,
		counts:    map[string]int{},
	}

	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: cliBridgeServerName, Version: "v1"}, nil)

	for _, decl := range conv.tools.decls.FunctionDeclarations {
		if decl == nil || skip[decl.Name] {
			continue
		}

		impl, ok := conv.tools.registry[decl.Name]
		if !ok {
			continue
		}

		server.AddTool(&sdkmcp.Tool{
			Name:        decl.Name,
			Description: decl.Description,
			InputSchema: declInputSchema(decl),
		}, bridge.handler(decl.Name, impl, conv.env))
	}

	var listenConfig net.ListenConfig

	// Loopback only, ephemeral port: reachable by the child this process
	// spawned and by nothing else on the network. Only a child in its OWN
	// network namespace needs more than that.
	address := "127.0.0.1:0"
	if reach == reachGateway {
		address = "0.0.0.0:0"
	}

	listener, err := listenConfig.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("cli bridge: listen: %w", err)
	}

	httpServer := &http.Server{
		// Stateless: the bridge outlives no client. It serves exactly one
		// child process for the length of one attempt, so per-session state
		// would only be a way for a crashed child to strand something.
		Handler: bridge.authenticated(sdkmcp.NewStreamableHTTPHandler(
			func(*http.Request) *sdkmcp.Server { return server },
			&sdkmcp.StreamableHTTPOptions{Stateless: true},
		)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	bridge.listener = listener
	bridge.url = bridgeURL(listener.Addr().String(), reach)
	bridge.server = httpServer

	// The goroutine closes over its OWN reference rather than reading
	// bridge.server: Close can run before this goroutine is scheduled, and a
	// Close that cleared the field would hand Serve a nil receiver.
	go func() {
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Debug("agent.cli.bridge.serve", "error", err)
		}
	}()

	return bridge, nil
}

// bridgeURL is the address to TELL the child, which is not always the one
// bound: a child in its own network namespace cannot dial this host's
// loopback, and the wildcard address that case binds is not a destination at
// all. A bind address that will not split is passed through as-is rather
// than guessed at — the child then fails to connect with a URL that at least
// says what happened.
func bridgeURL(bound string, reach bridgeReach) string {
	if reach != reachGateway {
		return "http://" + bound
	}

	_, port, err := net.SplitHostPort(bound)
	if err != nil {
		return "http://" + bound
	}

	return "http://" + net.JoinHostPort(shell.HostGatewayName, port)
}

// handler adapts one toolImpl to MCP. The adaptation is deliberately thin —
// the tool contract already says a failure is data, never a Go error, so the
// only translation needed is which shape of data counts as an error to the
// CLI.
func (b *cliBridge) handler(name string, impl toolImpl, env toolEnv) sdkmcp.ToolHandler {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			err := json.Unmarshal(req.Params.Arguments, &args)
			if err != nil {
				return nil, fmt.Errorf("tool %q: arguments are not a JSON object: %w", name, err)
			}
		}

		slog.Debug("agent.cli.bridge.call", "tool", name, "args", args)

		// Rejected before the impl runs, never after: the point of a budget is
		// bounding the side effect, which for ask_user is interrupting
		// somebody. The refusal goes back as ordinary tool-result data, the
		// same contract the HTTP path's executeBudgetedTool honours, so the
		// child reacts to it instead of the attempt aborting.
		if exhausted, budget := b.overBudget(name); exhausted {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{
					Text: fmt.Sprintf(`{"error": %q}`, fmt.Sprintf("%s: call budget (%d) exhausted for this attempt", name, budget)),
				}},
				IsError: true,
			}, nil
		}

		result := impl(ctx, args, env)

		b.capture(name, args, result)

		payload, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("tool %q: encoding result: %w", name, err)
		}

		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(payload)}},
			IsError: !requiredCallSucceeded(result),
		}, nil
	}
}

// overBudget reports whether name has already used its ceiling, counting this
// call against it when it has not.
//
// The check and the increment are one critical section on purpose: bridged
// calls arrive on the HTTP server's own goroutines, so a check-then-increment
// pair could let two concurrent asks both pass a budget of one.
func (b *cliBridge) overBudget(name string) (bool, int) {
	budget, capped := b.budgets[name]
	if !capped || budget <= 0 {
		return false, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.counts[name] >= budget {
		return true, budget
	}

	b.counts[name]++

	return false, budget
}

// capture records what a call means to the step, as opposed to what it means
// to the model. This is the half of the bridge that verdict routing depends
// on: the choice is read out of the tool's own result the moment it is
// produced, in this process.
func (b *cliBridge) capture(name string, args, result map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ok := requiredCallSucceeded(result)
	b.calls = append(b.calls, recordedToolCall{name: name, args: args, ok: ok})

	if !ok {
		return
	}

	b.satisfied[name] = true

	// Last successful call wins, matching how runAgentConversation resolves a
	// model that revises its own verdict.
	if verdict, isString := result["verdict"].(string); isString && verdict != "" {
		b.verdict = verdict
		b.note, _ = result["note"].(string)
	}
}

// authenticated rejects any request not carrying this bridge's token. The
// comparison is constant-time out of habit rather than necessity — a token
// this short-lived is not worth timing — and a failure says nothing about why.
func (b *cliBridge) authenticated(next http.Handler) http.Handler {
	expected := []byte("Bearer " + b.token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expected) != 1 {
			slog.Debug("agent.cli.bridge.unauthorized", "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// observed reports what the child did: the captured verdict/note, which
// required tools were satisfied, and every bridged call in order.
func (b *cliBridge) observed() (verdict, note string, satisfied map[string]bool, calls []recordedToolCall) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.verdict, b.note, maps.Clone(b.satisfied), append([]recordedToolCall(nil), b.calls...)
}

// writeConfig writes the --mcp-config document pointing the CLI at this
// bridge, and returns its path. It goes to the OS temp dir, never the step's
// workspace: the workspace is captured as artifacts and readable by the
// agent's own file tools, and a live callback URL belongs in neither.
func (b *cliBridge) writeConfig() (string, error) {
	document := map[string]any{
		"mcpServers": map[string]any{
			cliBridgeServerName: map[string]any{
				"type":    "http",
				"url":     b.url,
				"headers": map[string]any{"Authorization": "Bearer " + b.token},
			},
		},
	}

	payload, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("cli bridge: encoding mcp config: %w", err)
	}

	file, err := os.CreateTemp("", "steps-cli-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("cli bridge: creating mcp config: %w", err)
	}

	defer func() { _ = file.Close() }()

	_, err = file.Write(payload)
	if err != nil {
		return "", fmt.Errorf("cli bridge: writing mcp config: %w", err)
	}

	return file.Name(), nil
}

// Close stops serving. It is safe to call more than once.
//
// The step's context is deliberately stripped of cancellation first: a step
// killed by its timeout is exactly when the bridge still needs a moment to
// shut down cleanly, and an already-canceled context would skip that.
func (b *cliBridge) Close(ctx context.Context) error {
	var err error

	// Once rather than a nil-out: the field is read by the serving goroutine,
	// and clearing it was a data race that could panic Serve.
	b.closeOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cliBridgeShutdownTimeout)
		defer cancel()

		err = b.server.Shutdown(shutdownCtx)
		if err != nil {
			// Shutdown gives in-flight requests until the deadline; past it,
			// take the port back regardless.
			_ = b.listener.Close()

			err = fmt.Errorf("cli bridge: shutdown: %w", err)
		}
	})

	return err
}

// bridgedToolName is how a bridged tool is spelled in the CLI's own tool
// namespace — what --allowedTools must name to permit it.
func bridgedToolName(name string) string {
	return "mcp__" + cliBridgeServerName + "__" + name
}

// declInputSchema renders a tool declaration's parameters as a JSON Schema
// object. An MCP-backed tool already carries the server's own schema and is
// passed through untouched; everything else is converted from the genai
// schema the HTTP path uses.
func declInputSchema(decl *genai.FunctionDeclaration) any {
	if decl.ParametersJsonSchema != nil {
		return decl.ParametersJsonSchema
	}

	if decl.Parameters == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}

	return genaiSchemaToJSON(decl.Parameters)
}

// genaiSchemaToJSON converts the genai schema subset this package actually
// builds (see builtinAgentTools, buildVerdictTool, resolveToolSpec) into plain
// JSON Schema. Fields no tool here uses are deliberately not handled — a
// silent partial conversion of something richer would be worse than the
// obvious gap.
func genaiSchemaToJSON(schema *genai.Schema) map[string]any {
	out := map[string]any{}

	if schema.Type != "" {
		out["type"] = strings.ToLower(string(schema.Type))
	}

	if schema.Description != "" {
		out["description"] = schema.Description
	}

	if len(schema.Enum) > 0 {
		out["enum"] = append([]string{}, schema.Enum...)
	}

	if len(schema.Properties) > 0 {
		properties := make(map[string]any, len(schema.Properties))
		for name, property := range schema.Properties {
			properties[name] = genaiSchemaToJSON(property)
		}

		out["properties"] = properties
	}

	if schema.Items != nil {
		out["items"] = genaiSchemaToJSON(schema.Items)
	}

	if len(schema.Required) > 0 {
		out["required"] = append([]string{}, schema.Required...)
	}

	return out
}
