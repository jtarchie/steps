package resource

// Preflight: prove an mcp-backed resource type can actually call its tools
// before anything depends on it.
//
// The failure this exists for is quieter than a dead model. A `steps watch`
// whose trigger resource is misconfigured — a tool the server does not expose,
// or a tool whose required arguments the pipeline never sends — does not stop.
// It logs the check error and polls again, forever, on an interval, having
// never enqueued anything. Nothing is red; nothing is running; nothing is
// wrong except that the pipeline can never work. Both of those are yes/no
// facts about the server's published tool list, answerable in one connection
// before the first poll.
//
// What it does NOT do, said plainly: this checks the arguments' NAMES against
// the tool's `required` list. It cannot know whether a value renders to
// something the tool accepts, or whether the check's result will parse as a
// version array — those need a real call with a real version.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
)

// toolsCache remembers each server's tool list for the life of the process,
// bounded by the preflight cache window — the same bargain internal/agent's
// probeCache strikes, and for the same reason: a long-lived watcher must not
// pay a connection per poll, and a `steps run` triggered by it must not pay
// one it already paid a second ago.
//
//nolint:gochecknoglobals // process-lifetime memo, deliberately shared across runs
var toolsCache = &toolListCache{entries: map[string]toolListEntry{}}

type toolListEntry struct {
	at    time.Time
	tools []*sdkmcp.Tool
	err   error
}

type toolListCache struct {
	mu      sync.Mutex
	entries map[string]toolListEntry
}

func (c *toolListCache) lookup(key string, ttl time.Duration, now time.Time) (toolListEntry, bool) {
	if ttl <= 0 {
		return toolListEntry{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.at) > ttl {
		return toolListEntry{}, false
	}

	return entry, true
}

func (c *toolListCache) store(key string, entry toolListEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = entry
}

// ResetPreflightCache forgets every tool list this process has fetched. Tests
// use it to stay independent of each other.
func ResetPreflightCache() {
	toolsCache.mu.Lock()
	defer toolsCache.mu.Unlock()

	toolsCache.entries = map[string]toolListEntry{}
}

// Preflight checks the mcp-backed resource types behind the named resources
// and returns a problem per broken call. An empty result means every check/
// in/out tool those resources use exists on its server and will be called
// with at least the arguments it requires.
//
// job scopes the out: check to one job's own put steps (see Job.PutSteps): a
// `steps run` must not be blocked by how a job it is not running spells its
// put. A nil job judges every put in the pipeline, which is what `steps
// watch` wants — it will run them all.
//
// A shell-backed resource type checks nothing here: its check is a command,
// and whether that command works is what running it answers.
func Preflight(
	ctx context.Context, cfg *config.Config, job *config.Job,
	names []string, settings *config.Preflight,
) []config.Problem {
	var problems []config.Problem

	seen := map[string]bool{}

	for _, name := range names {
		if seen[name] {
			continue
		}

		seen[name] = true

		problems = append(problems, preflightResource(ctx, cfg, job, name, settings)...)
	}

	return problems
}

// preflightResource checks one resource's mcp: backend, if it has one.
func preflightResource(
	ctx context.Context, cfg *config.Config, job *config.Job,
	name string, settings *config.Preflight,
) []config.Problem {
	resource, err := cfg.FindResource(name)
	if err != nil {
		return nil // an unresolvable resource is already a load error
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return nil
	}

	switch resourceType.Config.Backend() {
	case config.BackendMCP:
	case config.BackendShell:
		// Nothing to prove ahead of time: a shell type's tools are whatever
		// is on PATH when it runs, which preflight cannot know from here.
		return nil
	}

	mcp := resourceType.Config.MCP

	tools, err := listToolsCached(ctx, cfg, mcp.Server, settings)
	if err != nil {
		// Transient: not answering is a fact about right now. The stage
		// problems below are facts about the pipeline — a tool that is not
		// there, arguments that cannot satisfy it — and no amount of waiting
		// changes either one.
		//
		// The exception is an oauth credential only a human can renew. It
		// arrives here looking exactly like a server that would not talk to
		// us, and treating it as waitable is how a watcher polls a server it
		// can never again authenticate to, quietly, until someone notices.
		return []config.Problem{{
			Target:    fmt.Sprintf("mcp %q", mcp.Server),
			Detail:    fmt.Sprintf("resource %q could not reach its server: %v", name, err),
			Transient: !errors.Is(err, stepsmcp.ErrNeedsLogin),
		}}
	}

	return resourceStageProblems(cfg, job, name, mcp, resource.Source, tools)
}

// resourceStageProblems checks the resource's lifecycle stages against the
// server's tool list — but only the stages the preflighted job actually
// reaches.
//
// Scoping matters as much here as it does for the out: payload below, and for
// the same reason: preflight refuses to START the job, so judging a stage the
// job never calls turns an irrelevant defect into a total block. A job whose
// only step is `get: eng-bugs` would otherwise be stopped by a typo in the
// out: tool it never publishes through, and a job whose only step is `put:
// eng-bugs` by a check: tool whose required arguments its source does not
// supply — a check that job never runs.
//
// job == nil means "no single job" (`steps watch`, which will run all of
// them), and then every stage is fair game.
func resourceStageProblems(
	cfg *config.Config, job *config.Job, name string, mcp *config.MCPResourceConfig,
	source map[string]any, tools []*sdkmcp.Tool,
) []config.Problem {
	target := fmt.Sprintf("resource %q", name)

	gets := job == nil || job.GetsResource(name)
	puts := job == nil || len(job.PutSteps(name)) > 0

	var problems []config.Problem

	// Each stage is optional, and a publish-only type declares no check: at
	// all (config.validateResourceGet rejects a get against one).
	if mcp.Check != nil && gets {
		problems = verifyStage(target, "check", *mcp.Check, sentArgNames(*mcp.Check, source), tools)
	}

	if mcp.In != nil && gets {
		problems = append(problems, verifyStage(target, "in", *mcp.In, sentArgNames(*mcp.In, source), tools)...)
	}

	if mcp.Out != nil && puts {
		problems = append(problems, verifyOutStage(cfg, job, target, name, *mcp.Out, tools)...)
	}

	return problems
}

// verifyOutStage checks the out: tool once per put step that targets this
// resource, because a put with no args: sends its OWN params: as the
// arguments — so "are the required arguments there" is a different question
// for each put. A resource nothing puts to is checked for tool existence
// only; there is no payload to judge.
func verifyOutStage(
	cfg *config.Config, job *config.Job, target, name string,
	call config.MCPToolCall, tools []*sdkmcp.Tool,
) []config.Problem {
	if call.Args != nil {
		return verifyStage(target, "out", call, sentArgNames(call, nil), tools)
	}

	puts := putSteps(cfg, job, name)
	if len(puts) == 0 {
		// Nothing publishes to this resource, so there is no payload to judge
		// — but the tool named still has to exist.
		return verifyStage(target, "out", call, nil, tools)
	}

	var (
		problems []config.Problem
		seen     = map[string]bool{}
	)

	for _, put := range puts {
		for _, problem := range verifyStage(target, "out", call, argNames(put.Params), tools) {
			// Several puts sending the same wrong payload is one problem to
			// fix, not one per step.
			if seen[problem.Detail] {
				continue
			}

			seen[problem.Detail] = true

			problems = append(problems, problem)
		}
	}

	return problems
}

// putSteps is the puts whose payload this preflight is entitled to judge:
// one job's own when a job is being preflighted, the whole pipeline's when
// there is no single job (see Preflight).
func putSteps(cfg *config.Config, job *config.Job, name string) []config.Step {
	if job != nil {
		return job.PutSteps(name)
	}

	return cfg.PutSteps(name)
}

// verifyStage reports what is wrong with one stage's tool call: a tool the
// server does not expose, or arguments the remote tool requires that this
// call will not send. A nil sent means the arguments are not knowable
// statically, so only the tool's existence is checked.
func verifyStage(target, stage string, call config.MCPToolCall, sent []string, tools []*sdkmcp.Tool) []config.Problem {
	tool := findTool(tools, call.Tool)
	if tool == nil {
		return []config.Problem{{
			Target: target,
			Detail: fmt.Sprintf("%s tool %q is not on the server (it offers: %v)", stage, call.Tool, toolNames(tools)),
		}}
	}

	if sent == nil {
		return nil
	}

	missing := missingRequired(requiredArgs(tool.InputSchema), sent)
	if len(missing) == 0 {
		return nil
	}

	return []config.Problem{{
		Target: target,
		Detail: fmt.Sprintf(
			"%s tool %q requires %v, which this call does not send (it sends: %v)\n    %s",
			stage, call.Tool, missing, sent, argAdvice(stage, call),
		),
	}}
}

// argAdvice says which knob fixes a missing argument, which differs by stage
// and by whether the call already names an args: mapping.
func argAdvice(stage string, call config.MCPToolCall) string {
	if call.Args != nil {
		return fmt.Sprintf("(add it to the resource type's mcp.%s.args:)", stage)
	}

	if stage == "out" {
		return fmt.Sprintf(
			"(the put's params: ARE the arguments when mcp.out.args: is unset — name it there, or map it in mcp.%s.args:)", stage)
	}

	return fmt.Sprintf(
		"(the resource's source: IS the argument object when mcp.%s.args: is unset — name it there, or map it in mcp.%s.args:)",
		stage, stage)
}

// sentArgNames lists the argument names a stage will send: the keys of its
// args: mapping when it has one, else the keys of the payload sent verbatim
// (see config.MCPToolCall). Sorted, because it is read by a human.
func sentArgNames(call config.MCPToolCall, fallback map[string]any) []string {
	if call.Args != nil {
		return argNames(call.Args)
	}

	return argNames(fallback)
}

// argNames sorts a payload's keys, never returning nil: an empty payload
// sends no arguments, which is a knowable fact worth checking, and nil means
// "not knowable" to verifyStage.
func argNames(payload map[string]any) []string {
	names := slices.Sorted(maps.Keys(payload))
	if names == nil {
		return []string{}
	}

	return names
}

// missingRequired returns the required argument names not present in sent.
func missingRequired(required, sent []string) []string {
	var missing []string

	for _, name := range required {
		if !slices.Contains(sent, name) {
			missing = append(missing, name)
		}
	}

	return missing
}

// requiredArgs reads a tool's `required` list out of its input schema.
//
// The schema arrives as whatever the JSON-RPC decode produced, so it is
// re-marshalled rather than type-asserted: a server that publishes no schema,
// or one this cannot read, reports nothing required — preflight's job is to
// catch a definite problem, never to invent one.
func requiredArgs(schema any) []string {
	if schema == nil {
		return nil
	}

	data, err := json.Marshal(schema)
	if err != nil {
		return nil
	}

	var parsed struct {
		Required []string `json:"required"`
	}

	err = json.Unmarshal(data, &parsed)
	if err != nil {
		return nil
	}

	return parsed.Required
}

func findTool(tools []*sdkmcp.Tool, name string) *sdkmcp.Tool {
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}

	return nil
}

func toolNames(tools []*sdkmcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}

	slices.Sort(names)

	return names
}

// listToolsCached connects to a server and lists its tools, once per cache
// window per server.
func listToolsCached(ctx context.Context, cfg *config.Config, server string, settings *config.Preflight) ([]*sdkmcp.Tool, error) {
	srv, err := cfg.FindMCPServer(server)
	if err != nil {
		//nolint:wrapcheck // FindMCPServer already names the server and lists the alternatives
		return nil, err
	}

	// Keyed on what the connection actually IS, not on the name it is
	// configured under: two pipelines (or two tests) can each call their
	// server "slack" while pointing at different endpoints. A stdio server's
	// identity is its whole invocation, so args/cwd are in the key too —
	// `npx server --repo a` and `--repo b` expose different tools.
	key := strings.Join(append([]string{srv.Name, srv.Endpoint, srv.Command, srv.Cwd}, srv.Args...), "|")
	now := time.Now()

	entry, found := toolsCache.lookup(key, settings.CacheWindow(), now)
	if found {
		slog.Debug("preflight.cached", "mcp_server", server)

		return entry.tools, entry.err
	}

	probeCtx, cancel := context.WithTimeout(ctx, settings.ProbeTimeout())
	defer cancel()

	tools, err := stepsmcp.ListServerTools(probeCtx, *srv)

	// Only OUR deadline is a timeout. When the caller's own context is the
	// one that ended, the run is being torn down (Ctrl-C, a canceled job) and
	// reporting it as "the server did not answer" blames the wrong party.
	if err != nil && probeCtx.Err() != nil && ctx.Err() == nil {
		err = fmt.Errorf("did not answer within %s", settings.ProbeTimeout())
	}

	// A failure caused by the CALLER going away says nothing about the
	// server, so it must not be remembered as though it did. Caching it would
	// let one Ctrl-C — or one canceled job under --max-concurrent — make every
	// other job touching this server fail with "context canceled" for the rest
	// of the cache window, with a healthy server the whole time.
	if err != nil && ctx.Err() != nil {
		return tools, err
	}

	toolsCache.store(key, toolListEntry{at: now, tools: tools, err: err})

	return tools, err
}
