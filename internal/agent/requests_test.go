package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestRequestLogTransportReportsHiddenRetries pins the whole point of
// requests.go: the LLM client retries a failing request twice underneath us,
// and until this transport existed those two extra requests appeared in no log
// anywhere. One failing turn cost three provider requests, not one, and the
// only record of that fact was a comment in a test.
func TestRequestLogTransportReportsHiddenRetries(t *testing.T) {
	statuses := []int{
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusInternalServerError,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statuses[0])
		statuses = statuses[1:]
	}))
	defer server.Close()

	buf := captureLogs(t)
	counter := &requestCounter{}
	client := &http.Client{Transport: &requestLogTransport{
		base: http.DefaultTransport, agent: "coder", model: "some-model",
	}}

	ctx := withRequestCounter(context.Background(), counter)

	for range 3 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()
	}

	if got := counter.Total(); got != 3 {
		t.Errorf("counter.Total() = %d, want 3 — this is the number that reaches retry.attempt_failed", got)
	}

	// Two lines, not three: the final failure of a burst is not followed by a
	// retry, and internal/retry reports it. Logging it here as well would
	// double-count the very figure this exists to make honest.
	if got := strings.Count(buf.String(), "agent.request_retry"); got != 2 {
		t.Errorf("agent.request_retry lines = %d, want 2\n%s", got, buf)
	}

	for _, want := range []string{"agent=coder", "model=some-model", "status=500", "attempt=1", "attempt=2", "of=3"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log does not name %q:\n%s", want, buf)
		}
	}
}

// TestRequestLogTransportSuccessResetsTheBurst verifies a retry burst is
// counted consecutively. A long conversation of good turns followed by one bad
// one must report "1 of 3", not the running total of every request it ever
// made.
func TestRequestLogTransportSuccessResetsTheBurst(t *testing.T) {
	statuses := []int{
		http.StatusInternalServerError,
		http.StatusOK,
		http.StatusInternalServerError,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statuses[0])
		statuses = statuses[1:]
	}))
	defer server.Close()

	buf := captureLogs(t)
	counter := &requestCounter{}
	client := &http.Client{Transport: &requestLogTransport{base: http.DefaultTransport, agent: "coder"}}

	ctx := withRequestCounter(context.Background(), counter)

	for range 3 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()
	}

	if got := counter.Total(); got != 3 {
		t.Errorf("counter.Total() = %d, want 3", got)
	}

	if got := strings.Count(buf.String(), "attempt=1"); got != 2 {
		t.Errorf("attempt=1 lines = %d, want 2 (the success between them resets the burst)\n%s", got, buf)
	}

	if strings.Contains(buf.String(), "attempt=2") {
		t.Errorf("the two failures are separate bursts, not a run of two:\n%s", buf)
	}
}

// TestRequestLogTransportIgnoresSuccess keeps the log quiet on the happy path:
// a working conversation must not narrate every turn.
func TestRequestLogTransportIgnoresSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	buf := captureLogs(t)
	counter := &requestCounter{}
	client := &http.Client{Transport: &requestLogTransport{base: http.DefaultTransport, agent: "coder"}}

	req, err := http.NewRequestWithContext(withRequestCounter(context.Background(), counter), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if buf.Len() != 0 {
		t.Errorf("a successful request logged something:\n%s", buf)
	}

	if got := counter.Total(); got != 1 {
		t.Errorf("counter.Total() = %d, want 1 — a success still costs a provider request", got)
	}
}

// TestRetryableStatus pins which responses the SDK retries, since the burst
// accounting is only correct if this matches openai-go's own rule.
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
