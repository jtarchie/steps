package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestOTelTracesARunAsItsStepTree: a run is one trace — the job's span at the root, each step under the container it ran in, a skip and a tolerated failure each saying so — and a command sees TRACEPARENT naming its own step's span.
func TestOTelTracesARunAsItsStepTree(t *testing.T) {
	collector := newOTLPCollector(t)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	seen := filepath.Join(dir, "traceparent")

	writePipelineFile(t, path, `jobs:
- name: build
  plan:
  - do:
    - task: hello
      run: echo "$TRACEPARENT" > `+seen+`
  - task: guarded
    when: "false"
    run: echo never
  - try:
      task: broken
      run: "false"
`)

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("steps run: %v", err)
	}

	spans, _ := collector.snapshot()
	job := findSpan(t, spans, tracepb.Span_SPAN_KIND_SERVER, "RUN build")
	do := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "do")
	hello := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "task hello")
	guarded := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "task guarded")
	broken := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "task broken")

	assertUnder(t, job, job, do)
	assertUnder(t, job, do, hello)
	assertUnder(t, job, job, guarded)
	assertUnder(t, job, nil, broken)

	if got := spanAttr(job, "cicd.pipeline.result"); got != "success" {
		t.Errorf("the run's result = %q, want success", got)
	}

	if got := spanAttr(guarded, "cicd.pipeline.task.run.result"); got != "skip" {
		t.Errorf("the guarded step's result = %q, want skip", got)
	}

	if got := spanAttr(broken, "cicd.pipeline.task.run.result"); got != "failure" || broken.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR {
		t.Errorf("the tolerated failure reads result=%q status=%s, want failure and an error status", got, broken.GetStatus().GetCode())
	}

	raw, err := os.ReadFile(seen) //nolint:gosec // this test's own temp file
	if err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf("00-%x-%x-01", hello.GetTraceId(), hello.GetSpanId())
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Errorf("the command saw TRACEPARENT=%q, want its own step's span %q", got, want)
	}
}

// assertUnder checks child is in the run's trace and, when parent is given, directly under it.
func assertUnder(t *testing.T, run, parent, child *tracepb.Span) {
	t.Helper()

	if string(child.GetTraceId()) != string(run.GetTraceId()) {
		t.Errorf("%s is not in the run's trace", child.GetName())
	}

	if parent != nil && string(child.GetParentSpanId()) != string(parent.GetSpanId()) {
		t.Errorf("%s is not under %s", child.GetName(), parent.GetName())
	}
}

func spanAttr(span *tracepb.Span, key string) string {
	for _, attr := range span.GetAttributes() {
		if attr.GetKey() == key {
			return attr.GetValue().GetStringValue()
		}
	}

	return ""
}

// TestOTelPutsADockerCallUnderItsStep: the docker client traces itself against whatever span the context carries, so the exec a containerized step makes must land under that step, not as a trace of its own.
func TestOTelPutsADockerCallUnderItsStep(t *testing.T) {
	requireDockerE2E(t)

	collector := newOTLPCollector(t)

	path := pipelinePath(t, t.TempDir())
	writePipelineFile(t, path, `jobs:
- name: build
  plan:
  - task: boxed
    image: alpine:3.20
    run: echo hi
`)

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("steps run: %v", err)
	}

	spans, _ := collector.snapshot()
	boxed := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "task boxed")

	for _, span := range spans {
		if span.GetKind() == tracepb.Span_SPAN_KIND_CLIENT && strings.HasSuffix(span.GetName(), "/exec") {
			if string(span.GetParentSpanId()) != string(boxed.GetSpanId()) {
				t.Errorf("the step's exec (%s) is not under the step's span", span.GetName())
			}

			return
		}
	}

	t.Fatal("no docker exec span was exported")
}
