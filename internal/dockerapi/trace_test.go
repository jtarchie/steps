package dockerapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// tracedDaemon is a daemon that answers just enough for a wait and a volume lookup, and the spans a client built the way New builds one left behind.
func tracedDaemon(t *testing.T) (*Client, *tracetest.InMemoryExporter) {
	t.Helper()

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/still-up/wait"):
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/wait"):
			_, _ = w.Write([]byte(`{"StatusCode":0}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(daemon.Close)

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	host := "tcp://" + strings.TrimPrefix(daemon.URL, "http://")

	api, err := connect(client.WithHost(host), client.WithTraceProvider(provider))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = api.Close() })

	return &Client{api: api, host: host}, exporter
}

func spanNames(exporter *tracetest.InMemoryExporter) []string {
	spans := exporter.GetSpans()
	names := make([]string, 0, len(spans))

	for _, span := range spans {
		names = append(names, span.Name)
	}

	return names
}

// The settle wait's timeout is its answer, so a healthy container must not leave an error span behind: it was 99% of the errors a backend showed.
func TestAHealthyContainerSettlingLeavesNoSpan(t *testing.T) {
	t.Parallel()

	docker, exporter := tracedDaemon(t)

	died, _, err := docker.SettleFor(t.Context(), "still-up", 50*time.Millisecond)
	if err != nil || died {
		t.Fatalf("SettleFor = %v, %v; want still up", died, err)
	}

	for _, span := range exporter.GetSpans() {
		if span.Status.Code == codes.Error || strings.Contains(span.Name, "wait") {
			t.Errorf("the settle wait left span %q (status %v)", span.Name, span.Status.Code)
		}
	}
}

// A real wait is still traced, and no call's span is named by an id, or a backend has one span name per container.
func TestEngineSpansAreNamedByRouteNotByID(t *testing.T) {
	t.Parallel()

	docker, exporter := tracedDaemon(t)

	result := docker.api.ContainerWait(t.Context(), "abc123", client.ContainerWaitOptions{})
	select {
	case <-result.Result:
	case err := <-result.Error:
		t.Fatal(err)
	}

	names := spanNames(exporter)
	if !slices.Contains(names, "POST /containers/{id}/wait") {
		t.Errorf("spans = %v; want POST /containers/{id}/wait", names)
	}

	for _, name := range names {
		if strings.Contains(name, "abc123") || strings.Contains(name, "_ping") || strings.Contains(name, "/v1.") {
			t.Errorf("span %q carries an id, a version or the ping", name)
		}
	}
}

func TestRouteOf(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]string{
		"/v1.47/containers/abc/wait":           "/containers/{id}/wait",
		"/v1.47/containers/create":             "/containers/create",
		"/v1.47/containers/json":               "/containers/json",
		"/containers/abc":                      "/containers/{id}",
		"/v1.47/exec/def/start":                "/exec/{id}/start",
		"/v1.47/volumes/steps-ws-1":            "/volumes/{name}",
		"/v1.47/volumes/create":                "/volumes/create",
		"/v1.47/images/create":                 "/images/create",
		"/v1.47/images/json":                   "/images/json",
		"/v1.47/images/alpine/git:latest/json": "/images/{name}/json",
		"/v1.47/images/ghcr.io/a/b@sha256:abc": "/images/{name}",
		"/v1.47/networks/n1":                   "/networks/{id}",
		"/_ping":                               "/_ping",
		"/v1.47/version":                       "/version",
	} {
		if got := routeOf(path); got != want {
			t.Errorf("routeOf(%q) = %q, want %q", path, got, want)
		}
	}
}
