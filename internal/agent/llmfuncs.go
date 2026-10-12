package agent

import (
	"context"
	"iter"
	"net/url"
	"slices"
	"time"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai/completions"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/model"
)

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

const telemetryScope = "github.com/jtarchie/steps/internal/agent"

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
	ctx, span := otel.Tracer(telemetryScope).Start(ctx, "chat "+modelName,
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
	meter := otel.Meter(telemetryScope)
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
