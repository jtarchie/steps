package agent

// The bridge that lets a CLI-backed agent step (see cli.go) use the tools its
// pipeline granted it.
//
// A CLI owns its own tool loop, so steps cannot hand it a tool registry the
// way it hands one to a model. What a CLI does accept is an MCP server. So
// the parent process becomes one, serving EVERY tool the step's grant
// produced — custom run: tools, mcp_servers: grants, the synthesized
// verdict/context tools, and (see issue #100) every built-in too, since
// `--tools ""` denies the CLI's own natives outright and the bridge is the
// tool surface, not a side channel next to a smaller native one.
//
// Two things fall out of this that are worth stating plainly. First, the tool
// implementations are the SAME ones an HTTP agent runs: path confinement,
// output caps and spilling, MCP subsetting and auth all apply unchanged,
// because nothing was reimplemented — including for a built-in that used to
// run as the CLI's own native tool. Second, a verdict call lands in the
// parent's memory as it happens, over a channel the model cannot forge — the
// CLI never has to be trusted to report what it decided, which is what keeps
// verdict routing meaningful across the process boundary.
//
// The residual trust window: the CLI's own binary decides what it offers the
// model at all, and `--tools ""` is policy inside a process this build does
// not pin. The init-event attestation in cliattest.go is the per-run proof
// that policy held — detection, not prevention, since it lands after the
// child has already parsed its own argv — with `--tools ""` remaining the
// primary fence.

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
)

// cliBridgeServerName is the MCP server name the child CLI knows the bridge
// by. It prefixes every bridged tool as `mcp__steps__<tool>` in the CLI's own
// namespace, which is also how cliArgs must spell them on --allowedTools.
const cliBridgeServerName = "steps"

// cliBridgeShutdownTimeout bounds how long Close waits for in-flight bridge
// requests. The child is already gone by then; this only covers a tool call
// that outlived it.
const cliBridgeShutdownTimeout = 5 * time.Second

// cliBridge is a running loopback MCP server exposing every tool this step's
// grant produced, plus what it observed the child do with them.
type cliBridge struct {
	server    *http.Server
	listener  net.Listener
	url       string
	closeOnce sync.Once
	// token authenticates the child. Loopback is not a permission boundary:
	// every process on the host can reach an open localhost port, and what
	// this one serves is now the step's ENTIRE tool surface — every built-in,
	// every custom run: tool (arbitrary shell in the workspace), every mcp:
	// grant, and the verdict tool that decides where the job goes next — not
	// a side channel next to a smaller native one. The child learns the token
	// from the mcp-config file, which is written 0600.
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
	// sees every call: internal/agent's turn loop enforces max_calls: on the
	// HTTP path, and a CLI agent does not run one, so this is where max_calls:
	// binds instead (see internal/config's checkCLIAgentTools, which no
	// longer refuses it for exactly this reason). max_questions: rides the
	// same machinery — it is denominated in a person's attention, not in tool
	// calls, so it is enforced here rather than by the turn loop either way.
	counts map[string]int
}

// newCLIBridge starts a bridge serving every tool in conv's registry. The
// caller must Close it.
//
// Always loopback: the CLI process is always a host subprocess of this one
// (see the issue #100 design note in cliexec.go), a bridged tool's own
// container — if `image:` names one — is where the TOOL runs, never where
// the CLI does, so there is no longer a child in its own network namespace
// that needs anything wider to dial back with.
//
// The conversation's per-tool call ceilings come along too, because this is
// the only place on the CLI path that sees every call — see cliBridge.counts.
func newCLIBridge(ctx context.Context, conv agentConversation) (*cliBridge, error) {
	bridge := &cliBridge{
		satisfied: map[string]bool{},
		token:     rand.Text(),
		budgets:   conv.tools.maxCalls,
		counts:    map[string]int{},
	}

	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: cliBridgeServerName, Version: "v1"}, nil)

	for _, decl := range conv.tools.decls.FunctionDeclarations {
		if decl == nil {
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
	// spawned and by nothing else on the network.
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
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
	bridge.url = "http://" + listener.Addr().String()
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

// cliBridgeToolPrefix is bridgedToolName's fixed prefix, factored out so
// debridgedToolName can strip exactly it without reconstructing the format
// string.
const cliBridgeToolPrefix = "mcp__" + cliBridgeServerName + "__"

// debridgedToolName reverses bridgedToolName for recording a trajectory (see
// clistream.go): `mcp__steps__read_file` becomes `read_file`, identical to
// what a hosted step's trajectory shows for the same call. A name with no
// such prefix — a surplus native the child called despite `--tools ""` — is
// returned VERBATIM rather than guessed at, which is deliberate: it is a
// second, human-readable signal of the same fence failure the init-event
// attestation exists to catch.
func debridgedToolName(name string) string {
	return strings.TrimPrefix(name, cliBridgeToolPrefix)
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
