package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jtarchie/steps/internal/agent/llmclient"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/usage"
)

// cliProviders is gen_ai.provider.name for a CLI source, whose endpoint the CLI picks.
var cliProviders = map[string]string{"claude": "anthropic"} //nolint:gochecknoglobals // static, read-only lookup table

// invocation is one invoke_agent span: a step's agent, a fix agent, or a delegation, emitted by whatever called it.
type invocation struct {
	span    trace.Span
	counts  *llmclient.Invocation
	started time.Time
}

// startInvocation scopes ctx to an agent invocation whose conversation is the run, so every agent a job reaches reads as one timeline.
func startInvocation(ctx context.Context, ri config.ResolvedInvocation) (context.Context, *invocation) {
	counts := &llmclient.Invocation{Agent: ri.AgentName, Conversation: events.RunID(ctx)}

	attrs := append([]attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.request.model", ri.ModelName),
	}, counts.Attributes()...)

	if ri.Description != "" {
		attrs = append(attrs, attribute.String("gen_ai.agent.description", ri.Description))
	}

	provider := llmclient.ProviderName(ri.BaseURL)
	if ri.CLI != "" {
		provider = cliProviders[ri.CLI]
	}

	if provider != "" {
		attrs = append(attrs, attribute.String("gen_ai.provider.name", provider))
	}

	ctx, span := otel.Tracer(llmclient.TelemetryScope).Start(ctx, "invoke_agent "+ri.AgentName, trace.WithAttributes(attrs...))

	return llmclient.WithInvocation(ctx, counts), &invocation{span: span, counts: counts, started: time.Now()}
}

// end closes the span with the invocation's own spend: a CLI agent has no chat spans, so this is the only place its tokens show.
func (i *invocation) end(ctx context.Context, spent usage.Step, err error) {
	if spent.Total > 0 {
		i.span.SetAttributes(
			attribute.Int64("gen_ai.usage.input_tokens", int64(spent.Prompt)),
			attribute.Int64("gen_ai.usage.output_tokens", int64(spent.Completion)),
			attribute.Int64("gen_ai.usage.cache_read.input_tokens", int64(spent.Cached)))
	}

	attrs := []attribute.KeyValue{attribute.String("gen_ai.agent.name", i.counts.Agent)}

	if err != nil {
		i.span.SetStatus(codes.Error, err.Error())
		i.span.SetAttributes(attribute.String("error.type", "_OTHER"))
		attrs = append(attrs, attribute.String("error.type", "_OTHER"))
	}

	meter := otel.Meter(llmclient.TelemetryScope)
	inference, tools := i.counts.Counts()

	duration, merr := meter.Float64Histogram("gen_ai.invoke_agent.duration", metric.WithUnit("s"),
		metric.WithDescription("How long an agent invocation took"),
		metric.WithExplicitBucketBoundaries(0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 12.8, 25.6, 51.2, 102.4, 204.8, 409.6))
	if merr == nil {
		duration.Record(ctx, time.Since(i.started).Seconds(), metric.WithAttributes(attrs...))
	}

	recordCalls(ctx, meter, "gen_ai.invoke_agent.inference_calls", "{inference_call}", "Model calls an agent invocation made itself", inference, attrs[:1])
	recordCalls(ctx, meter, "gen_ai.invoke_agent.tool_calls", "{tool_call}", "Tool calls an agent invocation made itself", tools, attrs[:1])

	i.span.End()
}

func recordCalls(ctx context.Context, meter metric.Meter, name, unit, description string, n int64, attrs []attribute.KeyValue) {
	calls, err := meter.Int64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(1, 2, 4, 8, 16, 32, 64, 128))
	if err == nil {
		calls.Record(ctx, n, metric.WithAttributes(attrs...))
	}
}

// toolSpan is one execute_tool span, for a tool the hosted loop dispatched or a CLI child called across the bridge.
type toolSpan struct {
	span      trace.Span
	name      string
	agent     []attribute.KeyValue
	agentName string
	started   time.Time
	capture   bool
}

func startToolSpan(ctx context.Context, name, id string, args map[string]any) (context.Context, *toolSpan) {
	inv := llmclient.InvocationFrom(ctx)
	inv.CountTool()

	ts := &toolSpan{name: name, agent: inv.Attributes(), started: time.Now(), capture: llmclient.CaptureContent()}
	if inv != nil {
		ts.agentName = inv.Agent
	}

	attrs := append([]attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", name),
		attribute.String("gen_ai.tool.type", "function"),
	}, ts.agent...)

	if id != "" {
		attrs = append(attrs, attribute.String("gen_ai.tool.call.id", id))
	}

	if ts.capture {
		attrs = append(attrs, attribute.String("gen_ai.tool.call.arguments", marshalSpanContent(args)))
	}

	ctx, ts.span = otel.Tracer(llmclient.TelemetryScope).Start(ctx, "execute_tool "+name, trace.WithAttributes(attrs...))

	return ctx, ts
}

// end marks a tool that answered {"error": ...} as failed — the tool contract's only failure — and leaves the invoke_agent above it alone, since a model that reads the error and recovers has not failed.
func (ts *toolSpan) end(ctx context.Context, response map[string]any) {
	attrs := []attribute.KeyValue{attribute.String("gen_ai.tool.name", ts.name)}
	if ts.agentName != "" {
		attrs = append(attrs, attribute.String("gen_ai.agent.name", ts.agentName))
	}

	if failure, ok := response["error"]; ok && failure != nil {
		ts.span.SetStatus(codes.Error, fmt.Sprint(failure))
		ts.span.SetAttributes(attribute.String("error.type", "_OTHER"))
		attrs = append(attrs, attribute.String("error.type", "_OTHER"))
	} else if ts.capture {
		ts.span.SetAttributes(attribute.String("gen_ai.tool.call.result", marshalSpanContent(response)))
	}

	duration, err := otel.Meter(llmclient.TelemetryScope).Float64Histogram("gen_ai.execute_tool.duration", metric.WithUnit("s"),
		metric.WithDescription("How long a tool call took"),
		metric.WithExplicitBucketBoundaries(0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92))
	if err == nil {
		duration.Record(ctx, time.Since(ts.started).Seconds(), metric.WithAttributes(attrs...))
	}

	ts.span.End()
}

func marshalSpanContent(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}

	return string(raw)
}
