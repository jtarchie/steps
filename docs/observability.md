# Observability

steps exports traces with [OpenTelemetry](https://opentelemetry.io), configured the standard way: the `OTEL_*` environment variables every collector and hosted backend documents. Nothing in a pipeline file and no flag turns it on, because where telemetry goes is the operator's decision, not the pipeline author's.

It is **off unless the environment names an exporter or an endpoint**. With neither set, nothing is installed and nothing is dialled.

It exports OTLP over HTTP (`http/protobuf`), which Honeycomb and every OpenTelemetry collector accept:

```sh
# Honeycomb (api.eu1.honeycomb.io for the EU region); the dataset is the service name, steps
OTEL_EXPORTER_OTLP_ENDPOINT=https://api.honeycomb.io \
OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=<your API key> \
steps web

# a local collector
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 steps run pipeline.yml
```

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Turns tracing on, exporting OTLP there |
| `OTEL_EXPORTER_OTLP_HEADERS` | Headers sent with every export, such as Honeycomb's `x-honeycomb-team` |
| `OTEL_TRACES_EXPORTER` | `otlp` turns tracing on (to `localhost:4318` with no endpoint); `none` keeps it off whatever else is set; anything else is refused |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf`, the default and the only one; `grpc` is refused rather than silently ignored |
| `OTEL_SDK_DISABLED=true` | Off, whatever else is set |
| `OTEL_SERVICE_NAME` | Overrides the default service name, `steps` |

Everything else the OpenTelemetry SDK reads (headers, sampling, resource attributes, batching) applies as documented there.

## What is traced

- **Every run is a trace.** The job is the root span, `RUN <job>`, carrying the pipeline, revision and run id. Each step is a span under the container it ran in — the same tree the [web transcript](web.md) draws — named by kind and name (`task test`, `do`, `try notify`). A step's span says how it ended (`cicd.pipeline.task.run.result`: `success`, `failure`, `error`, `cancellation` or `skip`); a failed step's span is marked as an error even when a `try:` tolerated it, and a skipped step's says why (`steps.skip.reason`, such as a cache hit or a false `when:`).
- **Commands continue the trace.** While tracing, every command a step runs — a task's `run:`, a resource's check/get/put, a CLI agent — gets `TRACEPARENT` naming its step's span, locally, in a container and on a worker, so anything in it that speaks OpenTelemetry joins the same trace.
- **Every request `steps web` answers**, named by route, such as `PUT /api/pipelines/:pipeline`.
- **The CLI's requests to a daemon** (`steps pipeline set` and the other verbs). The trace context travels with the request, so a set and the daemon's handling of it are one trace.
- **Docker engine calls and GCE calls**, from the clients steps already uses, which trace themselves once a provider is installed. A call a step makes sits under that step's span, named by the client's raw path (container ids included).

Log lines written under a traced request carry `trace_id` and `span_id`, so a line can be found from its trace and back.

What happens before a run's first event — validation, worker leases, image checks, preflight — is not yet inside its trace.
