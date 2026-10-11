package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/cli"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// otlpCollector is a fake OTLP/HTTP trace endpoint: it keeps every span it is sent, and the service.name each came under.
type otlpCollector struct {
	mu      sync.Mutex
	spans   []*tracepb.Span
	service map[*tracepb.Span]string
}

func newOTLPCollector(t *testing.T) *otlpCollector {
	t.Helper()

	collector := &otlpCollector{service: map[*tracepb.Span]string{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != "/v1/traces" {
			http.Error(w, "bad request", http.StatusBadRequest)

			return
		}

		var req collectortrace.ExportTraceServiceRequest

		err = proto.Unmarshal(body, &req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		collector.add(&req)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	return collector
}

func (c *otlpCollector) add(req *collectortrace.ExportTraceServiceRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, rs := range req.GetResourceSpans() {
		service := ""

		for _, attr := range rs.GetResource().GetAttributes() {
			if attr.GetKey() == "service.name" {
				service = attr.GetValue().GetStringValue()
			}
		}

		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				c.spans = append(c.spans, span)
				c.service[span] = service
			}
		}
	}
}

func (c *otlpCollector) snapshot() ([]*tracepb.Span, map[*tracepb.Span]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]*tracepb.Span(nil), c.spans...), c.service
}

// TestOTelTracesASetAcrossTheWire: with an OTLP endpoint in the environment, `steps pipeline set` and the daemon it talks to export one trace — the client's span, and the daemon's span for the route it handled, parented across the HTTP boundary.
func TestOTelTracesASetAcrossTheWire(t *testing.T) {
	collector := newOTLPCollector(t)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	reloadPipeline(t, path, "build")

	served := startWebFor(t, path)
	// Stopping is what flushes the daemon's spans; the set's were flushed when its command returned.
	served.stop(t)

	spans, service := collector.snapshot()
	server := findSpan(t, spans, tracepb.Span_SPAN_KIND_SERVER, "PUT /api/pipelines/:pipeline")
	client := findSpan(t, spans, tracepb.Span_SPAN_KIND_CLIENT, "HTTP PUT")

	if string(server.GetTraceId()) != string(client.GetTraceId()) || string(server.GetParentSpanId()) != string(client.GetSpanId()) {
		t.Errorf("the daemon's span is not the set's child: the trace context did not cross the wire")
	}

	if service[server] != "steps" || service[client] != "steps" {
		t.Errorf("service.name = %q (server), %q (client), want steps for both", service[server], service[client])
	}
}

func findSpan(t *testing.T, spans []*tracepb.Span, kind tracepb.Span_SpanKind, name string) *tracepb.Span {
	t.Helper()

	names := make([]string, 0, len(spans))

	for _, span := range spans {
		if span.GetKind() == kind && span.GetName() == name {
			return span
		}

		names = append(names, span.GetKind().String()+" "+span.GetName())
	}

	t.Fatalf("no %s span named %q among %q", kind, name, names)

	return nil
}

// TestOTelRefusesAProtocolItWouldIgnore: a grpc setting would otherwise be ignored by the HTTP exporter, and a misconfigured backend would look configured while receiving nothing.
func TestOTelRefusesAProtocolItWouldIgnore(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")

	path := pipelinePath(t, t.TempDir())
	reloadPipeline(t, path, "build")

	err := cli.Run([]string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), "grpc") {
		t.Fatalf("validate with OTLP over grpc = %v, want a refusal naming grpc", err)
	}
}
