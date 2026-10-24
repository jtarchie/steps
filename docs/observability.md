# Observability

steps exports traces and metrics with [OpenTelemetry](https://opentelemetry.io), configured the standard way: the `OTEL_*` environment variables every collector and hosted backend documents. Nothing in a pipeline file and no flag turns it on, because where telemetry goes is the operator's decision, not the pipeline author's.

It is **off unless the environment names an exporter or an endpoint**, per signal. With neither set, nothing is installed and nothing is dialled.

It exports OTLP over HTTP (`http/protobuf`), which Honeycomb and every OpenTelemetry collector accept:

```sh
# Honeycomb (api.eu1.honeycomb.io for the EU region); traces land in the dataset named by the service, steps
OTEL_EXPORTER_OTLP_ENDPOINT=https://api.honeycomb.io \
OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=<your API key> \
OTEL_EXPORTER_OTLP_METRICS_HEADERS=x-honeycomb-team=<your API key>,x-honeycomb-dataset=steps-metrics \
steps web

# a local collector
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 steps run pipeline.yml
```

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Turns traces and metrics on, exporting OTLP there |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `…_METRICS_ENDPOINT` | Turns on just that signal |
| `OTEL_EXPORTER_OTLP_HEADERS` / `…_METRICS_HEADERS` | Headers sent with every export, such as Honeycomb's `x-honeycomb-team`; Honeycomb also needs `x-honeycomb-dataset` on metrics |
| `OTEL_TRACES_EXPORTER` / `OTEL_METRICS_EXPORTER` | `otlp` turns the signal on (to `localhost:4318` with no endpoint); `none` keeps it off whatever else is set; anything else is refused |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf`, the default and the only one; `grpc` is refused rather than silently ignored |
| `OTEL_METRIC_EXPORT_INTERVAL` | Milliseconds between metric exports (default 60000) |
| `OTEL_SDK_DISABLED=true` | Off, whatever else is set |
| `OTEL_SERVICE_NAME` | Overrides the default service name, `steps` |
| `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true` | Puts prompts, responses and tool arguments and results on agent spans; off by default, see below |
| `OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT` | Truncates every span attribute to that many characters, captured content included — which can cut its JSON mid-message |

Everything else the OpenTelemetry SDK reads (headers, sampling, resource attributes, batching) applies as documented there.

## What is traced

- **Every run is a trace.** The job is the root span, `RUN <job>`, carrying the pipeline, revision and run id. Each step is a span under the container it ran in — the same tree the [web transcript](web.md) draws — named by kind and name (`task test`, `do`, `try notify`). A step's span says how it ended (`cicd.pipeline.task.run.result`: `success`, `failure`, `error`, `cancellation` or `skip`); a failed step's span is marked as an error even when a `try:` tolerated it, and a skipped step's says why (`steps.skip.reason`, such as a cache hit or a false `when:`).
- **Commands continue the trace.** While tracing, every command a step runs — a task's `run:`, a resource's check/get/put, a CLI agent — gets `TRACEPARENT` naming its step's span, locally, in a container and on a worker, so anything in it that speaks OpenTelemetry joins the same trace.
- **Every request `steps web` answers**, named by route, such as `PUT /api/pipelines/:pipeline` — including one the host check or [authentication](authentication.md) refuses.
- **The CLI's requests to a daemon** (`steps pipeline set` and the other verbs). The trace context travels with the request, so a set and the daemon's handling of it are one trace.
- **Agent steps, by the OpenTelemetry [GenAI conventions](https://github.com/open-telemetry/semantic-conventions-genai)**, in the shape Honeycomb's agent timeline reads. Each agent an agent step, a `fix:` or a delegation runs is an `invoke_agent <agent>` span carrying `gen_ai.agent.name`, its description, the requested model, the provider when the endpoint's host is one steps recognizes (`gen_ai.provider.name`; `anthropic` for a `@claude` source) and the tokens it spent; a delegation's span sits under the `execute_tool` call that made it. Each model call under it is a `chat <model>` span with the model the provider said answered (`gen_ai.response.model`), why it stopped (`gen_ai.response.finish_reasons`: `stop`, `tool_call`, `length`, `content_filter`) and the tokens it reported, cached reads included (`gen_ai.usage.cache_read.input_tokens`); each HTTP attempt it made, retries included, is a span under that. Each tool call is an `execute_tool <tool>` span, marked as an error when the tool returned one; the agent above it is not, since a model that reads the error and recovers has not failed. A CLI agent's model calls happen inside the CLI, so its `invoke_agent` carries the tokens the CLI reported and a tool it calls across the bridge is an `execute_tool` under it. The CLI is handed `TRACEPARENT` but none of the `OTEL_*` settings, so it exports no spans of its own beside these. A preflight probe's `chat` span names the agent it probed, with no `invoke_agent` above it. Every one of these spans carries `gen_ai.agent.name` and `gen_ai.conversation.id`, the run id, so every agent a run reaches reads as one conversation. A remote MCP server's request sits under the tool call that made it.
- **Prompts and responses are not exported unless `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true`.** With it, each `chat` span carries what was sent (`gen_ai.input.messages`, `gen_ai.system_instructions`) and what came back (`gen_ai.output.messages`), and each `execute_tool` its arguments and result, as JSON in the conventions' message shape. Nothing redacts them: a secret a tool printed or a prompt quoted is exported as-is, so turn it on only where the backend may hold what the [web transcript](web.md) holds. Drop or truncate it in a collector when it is too much. The flag does not cover a failed span's status message — a tool's error, a provider's rejection — which a failure is unreadable without; drop `status.message` in a collector if those can carry what must not leave.
- **Docker engine calls and GCE calls**, from the clients steps already uses, which trace themselves once a provider is installed. A call a step makes sits under that step's span, named by the client's raw path (container ids included).

Log lines written under a traced request carry `trace_id` and `span_id`, so a line can be found from its trace and back.

## What is measured

| Metric | Kind | Attributes | What it answers |
|---|---|---|---|
| `cicd.pipeline.run.duration` | histogram, seconds | `steps.pipeline`, `cicd.pipeline.name` (the job), `cicd.pipeline.result` | How long runs take, and — its count — how many finished, by result |
| `cicd.pipeline.run.active` | up-down counter | `steps.pipeline`, `cicd.pipeline.name` | How many runs are executing now |
| `steps.step.duration` | histogram, seconds | `steps.pipeline`, `steps.step.kind`, `cicd.pipeline.task.run.result`, `steps.skip.reason` on a skip | Which kinds of step are slow, and how often the cache saves work: the share of steps whose result is `skip` with a cache reason |
| `steps.queue.pending` | gauge | `steps.pipeline` | Builds `steps web` has queued and not yet started |
| `gen_ai.client.token.usage` | histogram, tokens | `gen_ai.request.model`, `gen_ai.token.type` (`input`/`output`) | What agents spend, per model; recorded only when the provider reported usage |
| `gen_ai.client.operation.duration` | histogram, seconds | `gen_ai.request.model`, `gen_ai.provider.name`, `error.type` on failure | How long model calls take |
| `gen_ai.invoke_agent.duration` | histogram, seconds | `gen_ai.agent.name`, `error.type` on failure | How long each agent takes, end to end |
| `gen_ai.invoke_agent.inference_calls` / `…tool_calls` | histogram, calls | `gen_ai.agent.name` | How many model and tool calls an agent makes per invocation; a delegation's own calls count against the sub-agent, not its caller |
| `gen_ai.execute_tool.duration` | histogram, seconds | `gen_ai.tool.name`, `gen_ai.agent.name`, `error.type` when the tool answered an error | Which tools are slow, and which fail |

No metric carries a run id or a step name, whose every value is new; a trace has both. The daemon's HTTP requests and the docker client's calls are measured too, under OpenTelemetry's standard `http.server.*` and `http.client.*` names.

A run's span opens before validation, worker leases, image pulls and preflight, so a run any of them refuses is still a trace — an errored `RUN` span, with whatever the refusal called (a preflight's model probe, an image pull) underneath — and is counted in the run metrics. What a step says while it runs (a retry's `attempt 2/3`, a worker starting, an image being pulled) is an event on its span.

`GET /healthz` answers `ok` to a platform's health check, without credentials, and is not traced.
