// openrouter.go wires OpenRouter's two prompt-caching levers into the agent
// LLM client. Neither is automatic through a stock OpenAI-compatible client,
// so a `model: openrouter/...` agent otherwise pays full input price on every
// turn of its tool-calling conversation — the case where the whole prior
// history is re-sent each turn and caching is worth the most.
//
//  1. x-session-id header, scoped to one agent within one job run: the run
//     token comes from the request context (see WithNewRun, set once per job
//     run by internal/pipeline) and the agent name from the client the call
//     was made through (see composeSessionID for why that split). OpenRouter
//     pins a session to one provider instance so the prompt cache stays warm;
//     without it, sticky routing only engages *after* a cache hit is observed,
//     which is too late for a short pipeline job.
//
//  2. Top-level cache_control: {type: ephemeral} body field, injected only
//     when the request's model routes to Anthropic. This is OpenRouter's
//     documented "automatic" caching form: it caches through the last
//     cacheable block and advances the boundary as the conversation grows,
//     rather than spending one of the four explicit per-message breakpoints.
//     Providers with implicit caching (OpenAI, Gemini, DeepSeek, Groq, ...)
//     need no marker and are left alone; sending them one risks a 400 for no
//     gain.
//
// Scope: the mutating transport is installed on the agent's HTTP client only
// when the resolved base URL is actually OpenRouter (see agentHTTPClient in
// provider.go), layered outside the every-provider repair transport
// (repair.go) on the shared process-wide base transport.
//
// The reference implementation this is ported from
// (jtarchie/topbanana's internal/model/openrouter_cache.go) instead swaps
// http.DefaultTransport process-wide, because the adk-utils-go version it
// targeted exposed only an HTTPOptions.Headers seam. v0.22.0 added
// HTTPOptions.Client, so that global is unnecessary here — which is just as
// well, since this repo's `reassign` linter forbids reassigning
// http.DefaultTransport/http.DefaultClient outright.
//
// The reference's third piece — teeing the response body to recover
// usage.prompt_tokens_details.cached_tokens — is deliberately not ported:
// steps tracks no token usage anywhere, so there is nothing to stitch the
// count onto.

package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxSessionIDLen is OpenRouter's documented cap on a session identifier.
const maxSessionIDLen = 256

// openRouterHost is the only host whose traffic gets the caching mutations.
const openRouterHost = "openrouter.ai"

// chatCompletionsPath is the only endpoint the mutations apply to. The
// adapter calls nothing else today; the check keeps a future non-chat call
// (a models listing, say) from being handed a cache_control body field it
// has no meaning for.
const chatCompletionsPath = "/chat/completions"

// maxLabelLen bounds each human-readable component of a session ID (the job
// name and the agent name). Two of them plus the random run token stay well
// inside maxSessionIDLen, so no pipeline can name its way past the cap.
const maxLabelLen = 64

// runIDKey types the context value holding one job run's identifier.
type runIDKey struct{}

// WithNewRun returns a derived context identifying a fresh run of jobName,
// which openRouterTransport combines with the calling agent's name to form the
// x-session-id header on OpenRouter chat completions (see composeSessionID).
//
// internal/pipeline calls this once per job run. The run token is random, so
// two runs of the same job — including two concurrent ones under `steps watch
// --max-concurrent` — never share a session and so never share a provider pin.
// It rides on the context rather than the client precisely so those concurrent
// runs stay separated while sharing everything else.
//
// The session is transport-level only: it never enters a step's merkle content
// (see internal/merkle), so it cannot invalidate a cached step.
func WithNewRun(ctx context.Context, jobName string) context.Context {
	return context.WithValue(ctx, runIDKey{}, sanitizeLabel(jobName)+"-"+rand.Text())
}

// runIDFromContext reads back what WithNewRun stored, or "" when the context
// carries no run (every non-pipeline caller, and every test that doesn't set
// one).
func runIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(runIDKey{}).(string)

	return id
}

// contextScopeKey types the context value holding the run-context write scope
// CHAIN — the enclosing blocks this context sits inside, outermost first, empty
// on the plain run.
type contextScopeKey struct{}

// WithContextScope derives a context whose recorded facts land under scope
// instead of the run id.
//
// It exists for concurrent branches: writes go somewhere only that branch
// touches, so two branches recording the same key cannot resolve to whichever
// finished last. internal/pipeline merges each branch's scope back at the join,
// in declaration order, under a key naming the branch (see mergeBranchContext).
//
// The scope is APPENDED to a chain rather than replacing one, because reads
// need the whole nesting and not just the innermost frame — see
// ContextReadScopes.
func WithContextScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, contextScopeKey{}, append(contextScopeChain(ctx), scope))
}

// ContextWriteScope reports where facts recorded on this context should land:
// the innermost branch scope when there is one, otherwise the run itself.
func ContextWriteScope(ctx context.Context) string {
	chain := contextScopeChain(ctx)
	if len(chain) == 0 {
		return runIDFromContext(ctx)
	}

	return chain[len(chain)-1]
}

// ContextReadScopes reports every scope a step on this context can see, from
// the run outward-in, so a nearer scope shadows a farther one.
//
// Writes and reads used to disagree about this. A branch wrote into its own
// scope and read from the run, so a step INSIDE a branch could not see what an
// earlier step of the same branch had just recorded — the facts became visible
// only at the join, by which point the branch had finished. Two steps outside a
// block do see each other, which is what made the asymmetry invisible until an
// across: matrix was nested in an in_parallel: branch.
//
// A concurrent SIBLING's writes stay invisible either way: they are in a scope
// that is on nobody else's chain.
func ContextReadScopes(ctx context.Context) []string {
	chain := contextScopeChain(ctx)
	scopes := make([]string, 0, len(chain)+1)

	return append(append(scopes, runIDFromContext(ctx)), chain...)
}

// contextScopeChain returns the write-scope chain carried on ctx, innermost
// last. A copy, so a caller appending to it cannot reach back into a context a
// sibling is also reading.
func contextScopeChain(ctx context.Context) []string {
	chain, _ := ctx.Value(contextScopeKey{}).([]string)

	return append([]string(nil), chain...)
}

// sanitizeLabel reduces a free-form name to characters legal in an HTTP header
// value and bounds its length. Job and agent names are arbitrary YAML strings
// that may contain spaces or non-ASCII; dropping to an unreserved ASCII subset
// is simpler than escaping and keeps the truncation below rune-safe.
func sanitizeLabel(name string) string {
	var out strings.Builder

	for _, r := range name {
		if out.Len() >= maxLabelLen {
			break
		}

		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
	}

	return out.String()
}

// composeSessionID builds the session identifier for one agent within one job
// run: the run token plus the agent's name.
//
// Scoping to the agent, rather than to the whole run, is deliberate. OpenRouter
// tracks sticky routing "per model, and per conversation", and a session is
// meant to be one conversation — but two different agents: entries have
// different models and different personas, so they share no cacheable prefix
// and are not one conversation. Two cases make the distinction matter rather
// than merely tidy:
//
//   - A router model (openrouter/auto and friends) pins the *resolved model*,
//     not just the provider, for the life of a session. Under a run-wide
//     session the first agent to run would decide the concrete model for every
//     later agent in the job.
//   - Distinct agents would otherwise be reported to OpenRouter as one
//     conversation whose prefix changes completely on every request.
//
// Keying on the agent name (not the step index) is what keeps the cases that
// *do* share a prefix together: a to:/verdicts: revise loop re-entering the
// same step, a sub-agent the parent calls repeatedly, and a fix: agent
// retrying all reuse one session, which per-step-invocation scoping would
// fragment.
//
// The session is stable for the life of a (run, agent) — there is no retry
// component. There used to be: a retry restarted the whole conversation, so
// the session was broken deliberately to keep the restart off the provider
// instance that had just failed, and little was lost because a fresh
// conversation could only ever have reused the short system+tools+prompt
// prefix anyway.
//
// That reasoning inverted when attempts: became a request-level retry (see
// requests.go). A retry now CONTINUES the same conversation, so holding the
// session is exactly what you want: the accumulated prefix stays cached and
// the retry is the cheapest it can be rather than the most expensive.
//
// An empty runID disables the header entirely — an agent run outside a job
// (tests, any future non-pipeline caller) sends no session.
func composeSessionID(runID, agentName string) string {
	if runID == "" {
		return ""
	}

	base := runID
	if agentName != "" {
		base += "-" + sanitizeLabel(agentName)
	}

	if len(base) > maxSessionIDLen {
		base = base[:maxSessionIDLen]
	}

	return base
}

// isOpenRouterBaseURL reports whether baseURL points at OpenRouter, and so
// whether the caching mutations apply at all. An unparsable URL, or any other
// provider (including a self-hosted gateway set via source.endpoint), reports
// false and gets a stock client.
//
// The host is lowercased first: url.Parse preserves whatever case the operator
// wrote, but DNS is case-insensitive, so a perfectly working
// `endpoint: https://OpenRouter.ai/api/v1/` would otherwise fail the match and
// silently disable caching for that agent with nothing to indicate why.
func isOpenRouterBaseURL(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}

	host := strings.ToLower(parsed.Hostname())

	return host == openRouterHost || strings.HasSuffix(host, "."+openRouterHost)
}

// isAnthropicModel reports whether an OpenRouter model slug routes to
// Anthropic, the only family here that needs an explicit cache_control marker.
// OpenRouter accepts both the plain "anthropic/<model>" form and the
// "~anthropic/<model>" form that forces Anthropic-direct routing (Bedrock and
// Vertex ignore the marker).
//
// The slug is matched rather than the endpoint because by this point
// config.resolveAgentTarget has already split "openrouter/anthropic/claude-x"
// into the OpenRouter base URL plus the model name "anthropic/claude-x" — the
// caller's model choice is the source of truth.
func isAnthropicModel(name string) bool {
	return strings.HasPrefix(name, "anthropic/") || strings.HasPrefix(name, "~anthropic/")
}

// openRouterTransport applies the session-ID header and cache_control body
// field to OpenRouter chat completions. Anything else — a non-chat path — is
// passed through to base untouched.
//
// agent is the name of the agents: entry this client was built for; it is
// fixed for the life of the client (one client per resolved invocation) and
// combines with the per-request run ID to form the session. Holding it here
// rather than in the context is what makes the session per-agent: a sub-agent
// or fix: agent gets its own client, and so its own session, without having to
// thread a second context value through the conversation loop.
type openRouterTransport struct {
	base  http.RoundTripper
	agent string
}

// CloseIdleConnections forwards to the wrapped transport, so an
// http.Client holding this wrapper can still release its sockets.
// http.Client.CloseIdleConnections type-asserts its Transport to an interface
// carrying this method; without it the call silently does nothing.
func (t *openRouterTransport) CloseIdleConnections() {
	closer, ok := t.base.(interface{ CloseIdleConnections() })
	if ok {
		closer.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper.
func (t *openRouterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	if req.URL == nil || !strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
		return base.RoundTrip(req) //nolint:wrapcheck // pass a non-chat request through verbatim; callers unwrap transport errors themselves
	}

	// RoundTrip must not modify the request it is handed, so mutate a clone.
	// The clone keeps the caller's own context — nothing here derives a new
	// one, so cancellation and deadlines propagate unchanged.
	ctx := req.Context()
	req = req.Clone(ctx)

	sessionID := composeSessionID(runIDFromContext(ctx), t.agent)
	if sessionID != "" {
		req.Header.Set("x-session-id", sessionID)
	}

	err := injectCacheControl(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter cache_control: %w", err)
	}

	return base.RoundTrip(req) //nolint:wrapcheck // surface the base transport's error verbatim, per the RoundTripper contract
}

// injectCacheControl rewrites req's body to carry a top-level cache_control
// marker when the model routes to Anthropic. The body is re-wrapped either
// way, because reading it to inspect the model consumed the original
// io.ReadCloser. GetBody is reset alongside it so openai-go's retry path can
// replay the request.
func injectCacheControl(req *http.Request) error {
	if req.Body == nil {
		return nil
	}

	body, err := io.ReadAll(req.Body)

	closeErr := req.Body.Close()

	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}

	if closeErr != nil {
		return fmt.Errorf("close request body: %w", closeErr)
	}

	body = withCacheControl(body)

	req.ContentLength = int64(len(body))
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	return nil
}

// ephemeralCacheControl is the marker value spliced in as-is, so the injected
// bytes are fixed rather than re-derived from a map on every request.
var ephemeralCacheControl = json.RawMessage(`{"type":"ephemeral"}`) //nolint:gochecknoglobals // immutable literal; never mutated after init

// withCacheControl returns body with a top-level cache_control marker added
// when its model field routes to Anthropic, and body unchanged otherwise.
//
// Decoding into map[string]json.RawMessage rather than map[string]any is what
// makes this a splice instead of a rewrite. The body is the whole conversation
// history — the large payload this feature exists to make cheaper — and every
// field other than the one we add is carried through as its original bytes:
// no deep decode of the message array, no numbers laundered through float64,
// no HTML-escaping of the <transition_context> blocks this codebase sends.
//
// A body that doesn't parse as a JSON object, or that re-marshals to an
// error, is passed through untouched rather than failing the request: this is
// an opportunistic cost optimization, and a caching tweak should never be the
// reason an agent step dies.
func withCacheControl(body []byte) []byte {
	var doc map[string]json.RawMessage

	err := json.Unmarshal(body, &doc)
	if err != nil {
		return body
	}

	var name string

	err = json.Unmarshal(doc["model"], &name)
	if err != nil || !isAnthropicModel(name) {
		return body
	}

	doc["cache_control"] = ephemeralCacheControl

	// json.Marshal would HTML-escape < > & even inside a json.RawMessage (it
	// compacts a custom marshaler's output with escaping on), which rewrites
	// every <transition_context> block this codebase sends. An encoder with
	// escaping disabled leaves the spliced-through bytes alone.
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	err = encoder.Encode(doc)
	if err != nil {
		return body
	}

	// Encode appends a newline; the request body should not carry one.
	return bytes.TrimRight(buf.Bytes(), "\n")
}
