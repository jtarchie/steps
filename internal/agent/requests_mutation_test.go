package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func requestsBodyServer(t *testing.T, statuses ...int) (*httptest.Server, func() []string) {
	t.Helper()

	var (
		mu     sync.Mutex
		bodies []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		i := min(len(bodies), len(statuses)-1)
		bodies = append(bodies, string(body))
		mu.Unlock()

		w.WriteHeader(statuses[i])
	}))
	t.Cleanup(server.Close)

	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), bodies...)
	}
}

// TestRequestRetryTransportReplaysTheBody: a retried POST must carry the same body, and one whose body cannot be rewound must not be retried at all, or the provider receives an empty request.
func TestRequestRetryTransportReplaysTheBody(t *testing.T) {
	captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 3}

	server, seen := requestsBodyServer(t, http.StatusServiceUnavailable, http.StatusOK)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if got := seen(); len(got) != 2 || got[0] != "payload" || got[1] != "payload" {
		t.Errorf("bodies seen = %q; want the payload twice (one failure, one retry)", got)
	}

	server, seen = requestsBodyServer(t, http.StatusServiceUnavailable)

	req, err = http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}

	req.GetBody = nil

	resp, err = transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if got := seen(); len(got) != 1 {
		t.Errorf("bodies seen = %q; want one request — a consumed body cannot be sent again", got)
	}
}

func TestRequestRetryTransportReportsAFailedRewind(t *testing.T) {
	captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 3}
	server, seen := requestsBodyServer(t, http.StatusServiceUnavailable)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}

	req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("body is gone") }

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	if err == nil || !strings.Contains(err.Error(), "body is gone") {
		t.Errorf("err = %v; want the rewind failure", err)
	}

	if got := seen(); len(got) != 1 {
		t.Errorf("requests = %d; want the first send only", len(got))
	}
}

// TestRequestRetryTransportAFirstSendIsNotARetry: the SDK stamps 0 on every first send, so an exhaustion recorded earlier in the conversation must not refuse the next fresh request.
func TestRequestRetryTransportAFirstSendIsNotARetry(t *testing.T) {
	captureLogs(t)

	counter := &requestCounter{}
	counter.setExhausted(errors.New("an earlier request ran out of attempts"))

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 1}
	server, count := countingServer(t, http.StatusOK)

	defer doRequest(t, transport, counter, server.URL).Body.Close() //nolint:errcheck // test teardown

	if count.Load() != 1 {
		t.Errorf("provider requests = %d; want the fresh send to reach the provider", count.Load())
	}
}
