# Observability

steps exports traces with [OpenTelemetry](https://opentelemetry.io), configured the standard way: the `OTEL_*` environment variables every collector and hosted backend documents. Nothing in a pipeline file and no flag turns it on, because where telemetry goes is the operator's decision, not the pipeline author's.

It is **off unless the environment names an exporter or an endpoint**. With neither set, nothing is installed and nothing is dialled.

```sh
# a collector, or any backend that accepts OTLP (Honeycomb, Grafana, Datadog, Jaeger)
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 steps web

# spans printed to stdout, for a quick look
OTEL_TRACES_EXPORTER=console steps run pipeline.yml
```

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Turns tracing on, exporting OTLP there |
| `OTEL_TRACES_EXPORTER` | `otlp`, `console`, or `none`; naming one turns tracing on, and `none` keeps it off whatever else is set |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` (the default) or `grpc` |
| `OTEL_SDK_DISABLED=true` | Off, whatever else is set |
| `OTEL_SERVICE_NAME` | Overrides the default service name, `steps` |

Everything else the OpenTelemetry SDK reads (headers, sampling, resource attributes, batching) applies as documented there.

## What is traced

- **Every request `steps web` answers**, named by route, such as `PUT /api/pipelines/:pipeline`.
- **The CLI's requests to a daemon** (`steps pipeline set` and the other verbs). The trace context travels with the request, so a set and the daemon's handling of it are one trace.
- **Docker engine calls and GCE calls**, from the clients steps already uses, which trace themselves once a provider is installed. Until builds are spans, each call is a trace of its own, named by the client's raw path (container ids included).

Log lines written under a traced request carry `trace_id` and `span_id`, so a line can be found from its trace and back.

Builds and steps are not spans yet.
