package agent

// Visibility for the LLM client's own transport-layer retry.
//
// There are two retry layers stacked on every agent step, and until this file
// existed only one of them was observable:
//
//	HTTP request retry     openai-go/v3, MaxRetries: 2   invisible, unconfigurable
//	whole-conversation     steps' own attempts:          logged, configurable
//
// They multiply. `provider requests per failing turn = attempts: x 3`, so
// attempts: 2 is up to six requests and attempts: 6 is up to eighteen — each
// one re-sending the entire conversation so far, which makes a retry late in a
// long conversation one of the most expensive things this system can do. A
// reader watching `attempt=1 attempts=2` scroll past reasonably concludes two
// failed calls. The real figure was six, and nothing said so.
//
// That matters commercially, not just aesthetically: providers that cap spend
// by the dollar rather than by request rate (the self-build experiment ran
// against one capped at $12/5h) can be exhausted in a fraction of the planned
// time by a multiplier nobody can see.
//
// This file makes the hidden layer observable. Configuring it is #20's job.

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// sdkRequestsPerCall mirrors openai-go/v3's MaxRetries: 2 default — one
// request plus two transport retries. It is a documented mirror of the
// dependency's behavior, NOT a setting: adk-utils-go's genai/openai Config
// exposes only APIKey/BaseURL/ModelName/HTTPOptions, so there is currently no
// way to change it from here. e2e_test.go pins the resulting request count, so
// a dependency bump that changes it fails the suite rather than silently
// re-pricing every failing call.
const sdkRequestsPerCall = 3

// retryableStatuses are the responses openai-go retries: request timeout,
// conflict, rate limit, and every 5xx.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	default:
		return code >= http.StatusInternalServerError
	}
}

// requestCounter tallies the provider requests one conversation attempt makes,
// and how many of those were consecutive retryable failures. Consecutive,
// because a burst is what a retry is: a success resets it, so a conversation
// of ten good turns followed by a failing one reports "1 of 3", not "11".
type requestCounter struct {
	mu          sync.Mutex
	total       int
	consecutive int
}

// record counts one round trip and reports the request's position within the
// current failure burst (1-based), or 0 when the request did not fail.
func (c *requestCounter) record(failed bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.total++

	if !failed {
		c.consecutive = 0

		return 0
	}

	c.consecutive++

	return c.consecutive
}

// Total is the number of provider requests counted so far.
func (c *requestCounter) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.total
}

type requestCounterKey struct{}

// withRequestCounter scopes a counter to one conversation attempt. Every retry
// callback that wraps a conversation installs one, so the count reported
// alongside a failed attempt describes that attempt alone.
func withRequestCounter(ctx context.Context, counter *requestCounter) context.Context {
	return context.WithValue(ctx, requestCounterKey{}, counter)
}

func requestCounterFrom(ctx context.Context) *requestCounter {
	counter, _ := ctx.Value(requestCounterKey{}).(*requestCounter)

	return counter
}

// requestLogTransport logs each provider request the client is about to retry.
// It sits in the transport stack because that is the only place the hidden
// retries are visible at all: the SDK reissues the whole request through this
// same http.Client, so every retry is another RoundTrip here.
type requestLogTransport struct {
	base  http.RoundTripper
	agent string
	model string
}

func (t *requestLogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()

	resp, err := t.base.RoundTrip(req)

	elapsed := time.Since(started)
	failed := err != nil || retryableStatus(resp.StatusCode)

	position := 0
	if counter := requestCounterFrom(req.Context()); counter != nil {
		position = counter.record(failed)
	} else if failed {
		position = 1
	}

	// The last request of a burst is not followed by a retry — its failure is
	// the one the caller sees and internal/retry reports. Logging it here too
	// would double-count the very number this exists to make honest.
	if !failed || position >= sdkRequestsPerCall {
		return resp, err //nolint:wrapcheck // a RoundTripper must return the transport's error unwrapped
	}

	fields := []any{
		"agent", t.agent,
		"model", t.model,
		"attempt", position,
		"of", sdkRequestsPerCall,
		"elapsed", elapsed,
	}

	if err != nil {
		fields = append(fields, "error", err)
	} else {
		fields = append(fields, "status", resp.StatusCode)
	}

	slog.Warn("agent.request_retry", fields...)

	return resp, err //nolint:wrapcheck // a RoundTripper must return the transport's error unwrapped
}
