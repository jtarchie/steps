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

Everything else the OpenTelemetry SDK reads (headers, sampling, resource attributes, batching) applies as documented there.

## What is traced

- **Every run is a trace.** The job is the root span, `RUN <job>`, carrying the pipeline, revision and run id. Each step is a span under the container it ran in — the same tree the [web transcript](web.md) draws — named by kind and name (`task test`, `do`, `try notify`). A step's span says how it ended (`cicd.pipeline.task.run.result`: `success`, `failure`, `error`, `cancellation` or `skip`); a failed step's span is marked as an error even when a `try:` tolerated it, and a skipped step's says why (`steps.skip.reason`, such as a cache hit or a false `when:`).
- **Commands continue the trace.** While tracing, every command a step runs — a task's `run:`, a resource's check/get/put, a CLI agent — gets `TRACEPARENT` naming its step's span, locally, in a container and on a worker, so anything in it that speaks OpenTelemetry joins the same trace.
- **Every request `steps web` answers**, named by route, such as `PUT /api/pipelines/:pipeline` — including one the host check or [authentication](authentication.md) refuses.
- **The CLI's requests to a daemon** (`steps pipeline set` and the other verbs). The trace context travels with the request, so a set and the daemon's handling of it are one trace.
- **Agent steps, by the OpenTelemetry GenAI conventions.** Each model call is a `chat <model>` span under its step, with the model, the provider's host, the finish reason and the tokens the provider reported (`gen_ai.usage.input_tokens`/`output_tokens`); each HTTP attempt it made, retries included, is a span under that. Each tool call is an `execute_tool <tool>` span, marked as an error when the tool returned one, and a remote MCP server's request sits under the tool call that made it. Prompts and responses are never exported — they belong to the [web transcript](web.md), and can carry secrets.
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
| `gen_ai.client.operation.duration` | histogram, seconds | `gen_ai.request.model`, `error.type` on failure | How long model calls take |

No metric carries a run id or a step name, whose every value is new; a trace has both. The daemon's HTTP requests and the docker client's calls are measured too, under OpenTelemetry's standard `http.server.*` and `http.client.*` names.

A run's span opens before validation, worker leases, image pulls and preflight, so a run any of them refuses is still a trace — an errored `RUN` span, with whatever the refusal called (a preflight's model probe, an image pull) underneath — and is counted in the run metrics. What a step says while it runs (a retry's `attempt 2/3`, a worker starting, an image being pulled) is an event on its span.

`GET /healthz` answers `ok` to a platform's health check, without credentials, and is not traced.
