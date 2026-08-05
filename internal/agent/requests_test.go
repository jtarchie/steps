package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// captureLogs redirects the default logger into a buffer for the duration of a
// test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return buf
}

// countingServer replies with the given statuses in order, repeating the last
// one, and counts the requests it received.
func countingServer(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var count atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(count.Add(1)) - 1
		if i >= len(statuses) {
			i = len(statuses) - 1
		}

		w.WriteHeader(statuses[i])
	}))
	t.Cleanup(server.Close)

	return server, &count
}

// doRequest issues one request through a client built on the retry transport.
func doRequest(t *testing.T, transport *requestRetryTransport, counter *requestCounter, url string) *http.Response {
	t.Helper()

	ctx := context.Background()
	if counter != nil {
		ctx = withRequestCounter(ctx, counter)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The SDK stamps this on every request it sends; a value of 0 means "this
	// is the first send", which is what a fresh request always is here.
	req.Header.Set(retryCountHeader, "0")

	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

// TestRequestRetryTransportSpendsExactlyAttempts is the whole redefinition in
// one assertion: attempts: N means N provider requests for a failing call, not
// N conversations and not N x 3 requests.
func TestRequestRetryTransportSpendsExactlyAttempts(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		want     int64
	}{
		{name: "unset behaves as one", attempts: 0, want: 1},
		{name: "one is a single request", attempts: 1, want: 1},
		{name: "three is three requests", attempts: 3, want: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, count := countingServer(t, http.StatusInternalServerError)
			captureLogs(t)

			counter := &requestCounter{}
			transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: test.attempts}

			defer doRequest(t, transport, counter, server.URL).Body.Close() //nolint:errcheck // test teardown

			if got := count.Load(); got != test.want {
				t.Errorf("provider requests = %d, want %d", got, test.want)
			}

			if got := counter.Total(); int64(got) != test.want {
				t.Errorf("counter.Total() = %d, want %d — this is the number a failed step reports", got, test.want)
			}
		})
	}
}

// TestRequestRetryTransportRefusesTheClientsOwnRetries is what keeps attempts:
// from silently meaning attempts: x 3 again. openai-go retries a failing
// request twice of its own accord unless the response says otherwise, and
// there is no way to configure that through the wrapper this project uses — so
// the transport sets the header the client honors.
func TestRequestRetryTransportRefusesTheClientsOwnRetries(t *testing.T) {
	server, _ := countingServer(t, http.StatusInternalServerError)
	captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 2}

	resp := doRequest(t, transport, &requestCounter{}, server.URL)
	defer resp.Body.Close() //nolint:errcheck // test teardown

	if got := resp.Header.Get(shouldRetryHeader); got != "false" {
		t.Errorf("%s = %q, want \"false\" — without it the client adds two more requests per call",
			shouldRetryHeader, got)
	}
}

// TestRequestRetryTransportStopsAtSuccess verifies a recovered request costs
// only what it needed.
func TestRequestRetryTransportStopsAtSuccess(t *testing.T) {
	server, count := countingServer(t, http.StatusInternalServerError, http.StatusOK)
	buf := captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 5}

	resp := doRequest(t, transport, &requestCounter{}, server.URL)
	defer resp.Body.Close() //nolint:errcheck // test teardown

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if got := count.Load(); got != 2 {
		t.Errorf("provider requests = %d, want 2 (one failure, then the success)", got)
	}

	if got := strings.Count(buf.String(), "agent.request_retry"); got != 1 {
		t.Errorf("agent.request_retry lines = %d, want 1\n%s", got, buf)
	}
}

// TestRequestRetryTransportLogsEveryRetry pins #2's contract under the new
// semantics: nothing about the request count is hidden. One line per retry —
// not per request, since the last failure is the one the caller is handed.
func TestRequestRetryTransportLogsEveryRetry(t *testing.T) {
	server, _ := countingServer(t, http.StatusInternalServerError)
	buf := captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", model: "some-model", attempts: 3}

	defer doRequest(t, transport, &requestCounter{}, server.URL).Body.Close() //nolint:errcheck // test teardown

	if got := strings.Count(buf.String(), "agent.request_retry"); got != 2 {
		t.Errorf("agent.request_retry lines = %d, want 2 for 3 requests\n%s", got, buf)
	}

	for _, want := range []string{"agent=coder", "model=some-model", "status=500", "attempt=1", "attempt=2", "attempts=3"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log does not name %q:\n%s", want, buf)
		}
	}
}

// TestRequestRetryTransportIgnoresSuccess keeps the happy path quiet.
func TestRequestRetryTransportIgnoresSuccess(t *testing.T) {
	server, count := countingServer(t, http.StatusOK)
	buf := captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 3}
	counter := &requestCounter{}

	defer doRequest(t, transport, counter, server.URL).Body.Close() //nolint:errcheck // test teardown

	if buf.Len() != 0 {
		t.Errorf("a successful request logged something:\n%s", buf)
	}

	if got := count.Load(); got != 1 {
		t.Errorf("provider requests = %d, want 1 — attempts: is a ceiling, not a quota", got)
	}

	if counter.Total() != 1 {
		t.Errorf("counter.Total() = %d, want 1", counter.Total())
	}
}

// TestRequestRetryTransportDoesNotRetryClientErrors verifies a 400 is taken at
// its word. Retrying a request the server has rejected on its merits spends
// money to be told the same thing again.
func TestRequestRetryTransportDoesNotRetryClientErrors(t *testing.T) {
	server, count := countingServer(t, http.StatusBadRequest)
	captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 3}

	defer doRequest(t, transport, &requestCounter{}, server.URL).Body.Close() //nolint:errcheck // test teardown

	if got := count.Load(); got != 1 {
		t.Errorf("provider requests = %d, want 1 — a 400 is not transient", got)
	}
}

// TestRequestRetryTransportSuppressesClientRetryOfAConnectionError covers the
// one path a response header cannot: when the request never reached a server
// there is no response to carry `x-should-retry: false`, and openai-go retries
// a connection error unconditionally. Left alone that would restore the
// multiplication — attempts: worth of dials, then two more rounds of the same.
func TestRequestRetryTransportSuppressesClientRetryOfAConnectionError(t *testing.T) {
	// A server that is closed immediately: every dial is refused.
	server, _ := countingServer(t)
	url := server.URL
	server.Close()

	captureLogs(t)

	counter := &requestCounter{}
	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 2}
	client := &http.Client{Transport: transport}
	ctx := withRequestCounter(context.Background(), counter)

	// Round 0 is the client's first send; it spends the transport's attempts.
	first, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	first.Header.Set(retryCountHeader, "0")

	_, err = client.Do(first) //nolint:bodyclose // every dial is refused, so there is no body
	if err == nil {
		t.Fatal("expected a connection error against a closed server")
	}

	if counter.Total() != 2 {
		t.Fatalf("provider requests = %d, want 2 (attempts:)", counter.Total())
	}

	// Rounds 1 and 2 are the client retrying that error. They must cost nothing.
	for round := 1; round <= 2; round++ {
		retry, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if reqErr != nil {
			t.Fatal(reqErr)
		}

		retry.Header.Set(retryCountHeader, "1")

		_, doErr := client.Do(retry) //nolint:bodyclose // every dial is refused, so there is no body
		if doErr == nil {
			t.Fatalf("round %d succeeded against a closed server", round)
		}
	}

	if got := counter.Total(); got != 2 {
		t.Errorf("provider requests = %d after the client's own retries, want 2 — attempts: is the whole budget", got)
	}
}

// TestRetryableStatus pins which responses are worth another request.
func TestRetryableStatus(t *testing.T) {
	retryable := []int{
		http.StatusRequestTimeout,
		http.StatusConflict,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	}

	for _, code := range retryable {
		if !retryableStatus(code) {
			t.Errorf("retryableStatus(%d) = false, want true", code)
		}
	}

	for _, code := range []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		if retryableStatus(code) {
			t.Errorf("retryableStatus(%d) = true, want false", code)
		}
	}
}
