package llmclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
func doRequest(t *testing.T, transport *requestRetryTransport, counter *RequestCounter, url string) *http.Response {
	t.Helper()

	ctx := context.Background()
	if counter != nil {
		ctx = WithRequestCounter(ctx, counter)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

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

			counter := &RequestCounter{}
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

// TestRequestRetryTransportStopsAtSuccess verifies a recovered request costs
// only what it needed.
func TestRequestRetryTransportStopsAtSuccess(t *testing.T) {
	server, count := countingServer(t, http.StatusInternalServerError, http.StatusOK)
	buf := captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 5}

	resp := doRequest(t, transport, &RequestCounter{}, server.URL)
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

	defer doRequest(t, transport, &RequestCounter{}, server.URL).Body.Close() //nolint:errcheck // test teardown

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
	counter := &RequestCounter{}

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

	defer doRequest(t, transport, &RequestCounter{}, server.URL).Body.Close() //nolint:errcheck // test teardown

	if got := count.Load(); got != 1 {
		t.Errorf("provider requests = %d, want 1 — a 400 is not transient", got)
	}
}

// TestRequestRetryTransportSpendsAttemptsOnAConnectionError: a refused dial has no response to read a status from, and is retried like a 5xx — attempts: of them and no more.
func TestRequestRetryTransportSpendsAttemptsOnAConnectionError(t *testing.T) {
	server, _ := countingServer(t)
	url := server.URL
	server.Close()

	captureLogs(t)

	counter := &RequestCounter{}
	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 2}

	req, err := http.NewRequestWithContext(WithRequestCounter(context.Background(), counter), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = (&http.Client{Transport: transport}).Do(req) //nolint:bodyclose // every dial is refused, so there is no body
	if err == nil {
		t.Fatal("expected a connection error against a closed server")
	}

	if got := counter.Total(); got != 2 {
		t.Errorf("provider requests = %d, want 2 (attempts:)", got)
	}
}

// headerServer answers each request with the next status and headers, repeating the last.
func headerServer(t *testing.T, replies ...func(http.ResponseWriter)) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var count atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		replies[min(int(count.Add(1))-1, len(replies)-1)](w)
	}))
	t.Cleanup(server.Close)

	return server, &count
}

func reply(status int, headers ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}

		w.WriteHeader(status)
	}
}

// TestRequestRetryTransportGivesUpWhenAskedToWaitTooLong: a provider saying "come back in an hour" is not answered in half a second, and retrying it early only spends the attempt — the response goes back, and fallback: can move on.
func TestRequestRetryTransportGivesUpWhenAskedToWaitTooLong(t *testing.T) {
	server, count := headerServer(t, reply(http.StatusTooManyRequests, "Retry-After", "3600"))
	captureLogs(t)

	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 3}

	resp := doRequest(t, transport, &RequestCounter{}, server.URL)
	defer resp.Body.Close() //nolint:errcheck // test teardown

	if got := count.Load(); got != 1 || resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("requests = %d, status %d; want the 429 back after 1", got, resp.StatusCode)
	}
}

// TestRequestRetryTransportHonorsTheProvidersRetryHint: x-should-retry is OpenAI's own word on a response, above its status, both ways.
func TestRequestRetryTransportHonorsTheProvidersRetryHint(t *testing.T) {
	captureLogs(t)

	server, count := headerServer(t, reply(http.StatusBadRequest, "x-should-retry", "true", "Retry-After-Ms", "1"), reply(http.StatusOK))
	transport := &requestRetryTransport{base: http.DefaultTransport, agent: "coder", attempts: 3}

	resp := doRequest(t, transport, &RequestCounter{}, server.URL)
	_ = resp.Body.Close()

	if got := count.Load(); got != 2 || resp.StatusCode != http.StatusOK {
		t.Errorf("a 400 marked retryable: requests = %d, status %d; want the 200 after 2", got, resp.StatusCode)
	}

	server, count = headerServer(t, reply(http.StatusServiceUnavailable, "x-should-retry", "false"))

	resp = doRequest(t, transport, &RequestCounter{}, server.URL)
	_ = resp.Body.Close()

	if got := count.Load(); got != 1 {
		t.Errorf("a 503 marked not retryable: requests = %d, want 1", got)
	}
}

// TestRetryDelay: the provider's own Retry-After-Ms or Retry-After wins over the linear backoff, up to retryAfterLimit; past it, no retry at all.
func TestRetryDelay(t *testing.T) {
	t.Parallel()

	soon := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)

	tests := []struct {
		name    string
		headers []string
		want    time.Duration
		retry   bool
	}{
		{"no header is the linear backoff", nil, 2 * retryBackoffUnit, true},
		{"milliseconds", []string{"Retry-After-Ms", "1500"}, 1500 * time.Millisecond, true},
		{"seconds", []string{"Retry-After", "2"}, 2 * time.Second, true},
		{"fractional seconds", []string{"Retry-After", "0.25"}, 250 * time.Millisecond, true},
		{"milliseconds win over seconds", []string{"Retry-After-Ms", "10", "Retry-After", "9"}, 10 * time.Millisecond, true},
		{"an HTTP date", []string{"Retry-After", soon}, 30 * time.Second, true},
		{"past the limit is no retry", []string{"Retry-After", "121"}, 0, false},
		{"garbage is the linear backoff", []string{"Retry-After", "soon"}, 2 * retryBackoffUnit, true},
		{"negative is the linear backoff", []string{"Retry-After-Ms", "-5"}, 2 * retryBackoffUnit, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{Header: http.Header{}}
			for i := 0; i+1 < len(test.headers); i += 2 {
				resp.Header.Set(test.headers[i], test.headers[i+1])
			}

			got, retry := retryDelay(resp, 2)
			if retry != test.retry || (got != test.want && (got < test.want-2*time.Second || got > test.want)) {
				t.Errorf("retryDelay = %v, %v; want %v, %v", got, retry, test.want, test.retry)
			}
		})
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

// TestIsTransientProviderError pins the classifier the mid-run fallback:
// cascade decides on: a connection-level failure or a retryable status is
// eligible to fail over, a model's own rejection (a 4xx) is not — and neither
// is an internal error this package raises itself (a budget breach, a
// malformed response), since runOneConversation's returned error isn't
// necessarily the provider's fault at all. dialErr/wrappedDialErr use a real
// *net.OpError, the shape an actual connection failure takes reaching here
// (via net/http's client) — a bare errors.New with dial-shaped TEXT is not a
// net.Error and must not pass.
//
// Neither context sentinel is transient: a canceled run says nothing about
// the provider, and a spent deadline cannot be told apart from work that was
// simply too big for the budget. ClassifySource resolves both to
// sourceUnproven — see cascade.go, and TestClassifySource for the rows.
func TestIsTransientProviderError(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"5xx api error", &APIError{StatusCode: http.StatusInternalServerError}, true},
		{"429 api error", &APIError{StatusCode: http.StatusTooManyRequests}, true},
		{"400 api error", &APIError{StatusCode: http.StatusBadRequest}, false},
		{"401 api error", &APIError{StatusCode: http.StatusUnauthorized}, false},
		{"connection error", dialErr, true},
		{"wrapped connection error", fmt.Errorf("agent: generate content: %w", dialErr), true},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"body truncated mid-response", fmt.Errorf("decode response: %w", io.ErrUnexpectedEOF), true},
		// io.EOF is the stdlib's ordinary end-of-stream, returned by every
		// read that finishes. Reading it as an outage let any normal
		// termination anywhere in this package cascade to another model.
		{"a bare io.EOF is not an outage", fmt.Errorf("decode response: %w", io.EOF), false},
		// A provider that sends malformed JSON has a problem, but not one
		// another SOURCE fixes, and encoding/json returns this shape for
		// every decode in the process — including model-authored tool args.
		{"a json syntax error is not an outage", fmt.Errorf("decode response: %w", &json.SyntaxError{}), false},
		{"budget exceeded is an internal error, not the provider's fault", errors.New("agent budget exceeded (spent 500 tokens)"), false},
		{"malformed/empty response is an internal error, not a connection failure", errors.New("agent: model returned an empty response"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientProviderError(tt.err); got != tt.want {
				t.Errorf("isTransientProviderError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
