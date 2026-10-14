// Package llmclient builds the model.LLM an agent step talks to and the HTTP transports under it: retries, tool-call argument repair, OpenRouter's caching levers, and the choice of which fallback source to try next.
package llmclient

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai/completions"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
)

// New constructs the model.LLM used for real runs, given the
// already-resolved base URL/model/key (see config.ResolveAgentInvocation).
// Returning the model.LLM interface (not the concrete *openai.Model) keeps
// runAgentConversation testable against an in-process fake.
//
// Every invocation gets the package's shared HTTP client (see
// agentHTTPClient): the tool-call argument repair transport applies to all
// providers, and an OpenRouter or other caching-gateway base URL additionally
// gets the session/cache transport (see openrouter.go, gateway.go).
//
// It takes the whole ResolvedInvocation rather than loose strings so the
// mapping from invocation fields to client settings lives in exactly one
// place — the three call sites (PreparedStep.Run, RunFix, buildSubAgentTool) can't
// individually mix up which field feeds which parameter.
func New(ri config.ResolvedInvocation, apiKey string) model.LLM {
	cfg := genaiopenai.Config{
		APIKey:    apiKey,
		BaseURL:   ri.BaseURL,
		ModelName: ri.ModelName,
		Dialect:   dialectFor(ri.ModelName),
		HTTPOptions: genaiopenai.HTTPOptions{
			Client: agentHTTPClient(ri),
		},
	}

	return newLLMFuncs(genaiopenai.New(cfg), ri.BaseURL)
}

// agentHTTPClient returns the *http.Client an invocation's LLM uses. Its
// transport stack is, innermost out: the shared base transport (one
// process-wide connection pool, see agentBaseTransport), the request-retry
// transport that implements attempts: (see requests.go — innermost, because an
// individual request is the operation it retries), the tool-call argument
// repair transport (all providers — see repair.go), and, only for an
// OpenRouter or caching-gateway base URL, the session/cache transport. Split
// out from New so the field mapping it performs — the session is
// scoped by AgentName, not ModelName — is directly assertable in a test.
func agentHTTPClient(ri config.ResolvedInvocation) *http.Client {
	retrying := &requestRetryTransport{
		// Under the retries, so each attempt is its own HTTP span inside the turn's chat span.
		base:     otelhttp.NewTransport(agentBaseTransport()),
		agent:    ri.AgentName,
		model:    ri.ModelName,
		attempts: ri.Attempts,
	}

	var transport http.RoundTripper = &repairTransport{base: retrying}

	if isOpenRouterBaseURL(ri.BaseURL) {
		transport = &openRouterTransport{base: transport, agent: ri.AgentName}
	} else if gateway, ok := cachingGatewayFor(ri.BaseURL); ok {
		transport = &gatewayTransport{base: transport, agent: ri.AgentName, gateway: gateway}
	}

	return &http.Client{Transport: transport}
}

// LookupAPIKey reads the API key from the OS environment variable named by
// envVar. When required, a missing/empty variable (or a missing envVar
// name) is a hard error — sending a blank Authorization header would just
// produce a confusing 401 from the endpoint. When not required (local
// providers with no default key), a missing key resolves to "" and no
// Authorization header is sent.
func LookupAPIKey(envVar string, required bool) (string, error) {
	if envVar == "" {
		if required {
			return "", errors.New("agent source is missing api_key_env")
		}

		return "", nil
	}

	val, ok := os.LookupEnv(envVar)
	if !ok || val == "" {
		if required {
			return "", fmt.Errorf("environment variable %q (api_key_env) is not set", envVar)
		}

		return "", nil
	}

	return val, nil
}

// dialectFor picks the client's provider dialect by model name rather than endpoint, since one gateway (OpenCode, OpenRouter) serves many providers; nil keeps the adapter OpenAI-pure, which is every model but DeepSeek's.
func dialectFor(modelName string) genaiopenai.Dialect {
	if strings.Contains(strings.ToLower(modelName), "deepseek") {
		return deepSeekDialect{}
	}

	return nil
}

// agentResponseHeaderTimeout mirrors the bound openai-go's own
// defaultHTTPClient applies when the caller supplies no client. Supplying
// one (which every agent now does, for the repair transport) opts out of
// that default, so it is re-applied here rather than silently dropping
// stuck-connection protection.
const agentResponseHeaderTimeout = 10 * time.Minute

// agentBaseTransport is the transport every agent LLM client shares, built
// once. It reproduces openai-go's defaultHTTPClient (clone
// http.DefaultTransport, bound the wait for response headers) and is
// process-wide rather than per-client: only the thin per-agent wrappers
// (repair, and for OpenRouter the session/cache transport) differ per
// agent, so every agent step, sub-agent, and fix agent reuses one
// connection pool instead of paying a fresh TLS handshake each.
//
// Cloning once (rather than using http.DefaultTransport directly) keeps the
// response-header bound off every other HTTP user in the process
// (internal/mcp, resource types shelling out, ...).
var agentBaseTransport = sync.OnceValue(func() http.RoundTripper { //nolint:gochecknoglobals // process-wide connection pool, built lazily on first agent
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}

	transport = transport.Clone()
	transport.ResponseHeaderTimeout = agentResponseHeaderTimeout

	return transport
})

// llmFuncs holds method values, not the model: adk-utils' Model in an interface keeps all of openai-go linked (~8MB) via its client field.
type llmFuncs struct {
	name     func() string
	generate func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error]
	// server is the provider's host, for the GenAI conventions' server.address.
	server string
}

func newLLMFuncs(m *genaiopenai.Model, baseURL string) llmFuncs {
	server := ""

	parsed, err := url.Parse(baseURL)
	if err == nil {
		server = parsed.Hostname()
	}

	return llmFuncs{name: m.Name, generate: m.GenerateContent, server: server}
}

func (f llmFuncs) Name() string { return f.name() }

// GenerateContent is one model call, traced as a GenAI chat span whose context the HTTP request inherits, so a retried attempt shows under the turn it belongs to.
func (f llmFuncs) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		ctx, turn := startChat(ctx, f.name(), f.server)

		for resp, err := range f.generate(ctx, req, stream) {
			turn.observe(resp, err)

			if !yield(resp, err) {
				break
			}
		}

		turn.end(ctx)
	}
}

// TelemetryScope names the tracer and meter. It is still the agent package's path, as it was before this layer moved out of it, so a dashboard keyed on the scope keeps matching.
const TelemetryScope = "github.com/jtarchie/steps/internal/agent"

type chatTurn struct {
	span    trace.Span
	model   string
	started time.Time
	input   int64
	output  int64
	// reported is whether any response carried usage; a provider that omits it must not read as a call that cost nothing.
	reported bool
	failure  error
}

func startChat(ctx context.Context, modelName, server string) (context.Context, *chatTurn) {
	ctx, span := otel.Tracer(TelemetryScope).Start(ctx, "chat "+modelName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.request.model", modelName),
			attribute.String("server.address", server),
		))

	return ctx, &chatTurn{span: span, model: modelName, started: time.Now()}
}

// observe takes usage only as the provider reported it, the same rule stepUsage.record keeps.
func (c *chatTurn) observe(resp *model.LLMResponse, err error) {
	if err != nil {
		c.failure = err

		return
	}

	if resp == nil {
		return
	}

	if resp.ModelVersion != "" {
		c.span.SetAttributes(attribute.String("gen_ai.response.model", resp.ModelVersion))
	}

	if resp.FinishReason != "" {
		c.span.SetAttributes(attribute.StringSlice("gen_ai.response.finish_reasons", []string{string(resp.FinishReason)}))
	}

	if usage := resp.UsageMetadata; usage != nil {
		c.reported = true
		c.input += int64(usage.PromptTokenCount)
		c.output += int64(usage.CandidatesTokenCount)
	}
}

func (c *chatTurn) end(ctx context.Context) {
	meter := otel.Meter(TelemetryScope)
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", c.model),
	}

	if c.reported {
		c.recordTokens(ctx, meter, attrs)
	}

	if c.failure != nil {
		c.span.SetStatus(codes.Error, c.failure.Error())
		attrs = append(attrs, attribute.String("error.type", "_OTHER"))
	}

	duration, err := meter.Float64Histogram("gen_ai.client.operation.duration", metric.WithUnit("s"),
		metric.WithDescription("How long a model call took"),
		metric.WithExplicitBucketBoundaries(0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92))
	if err == nil {
		duration.Record(ctx, time.Since(c.started).Seconds(), metric.WithAttributes(attrs...))
	}

	c.span.End()
}

func (c *chatTurn) recordTokens(ctx context.Context, meter metric.Meter, attrs []attribute.KeyValue) {
	c.span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", c.input), attribute.Int64("gen_ai.usage.output_tokens", c.output))

	tokens, err := meter.Int64Histogram("gen_ai.client.token.usage", metric.WithUnit("{token}"),
		metric.WithDescription("Tokens a model call used, as the provider reported them"),
		metric.WithExplicitBucketBoundaries(1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864))
	if err != nil {
		return
	}

	tokens.Record(ctx, c.input, metric.WithAttributes(slices.Concat(attrs, []attribute.KeyValue{attribute.String("gen_ai.token.type", "input")})...))
	tokens.Record(ctx, c.output, metric.WithAttributes(slices.Concat(attrs, []attribute.KeyValue{attribute.String("gen_ai.token.type", "output")})...))
}
