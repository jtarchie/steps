package llmclient

// The agent step's request retry: attempts: applied where it belongs.
//
// `attempts:` used to mean two different operations depending on the step it
// sat on. On a task it retried a command. On an agent it threw away every turn
// of an accumulated conversation and started over — amnesia, not retry — at
// three orders of magnitude more cost, against a failure the transport layer
// had already retried and concluded was not transient. The workspace survived
// a restart but the memory did not, so a restarted attempt inherited its own
// half-finished edits with no recollection of making them; that incoherence
// needed prompt text to work around, which is a design smell, not a prompting
// problem. Across a real self-build experiment it fired five times and never
// once changed the outcome.
//
// So attempts: now means what it means everywhere else: retry the failing
// operation. For an agent, the failing operation is one HTTP request, and this
// file is where that happens.
//
// Doing it here rather than around the conversation is what makes the count
// honest: this is the only retry under an agent's call, so a retry loop above
// it would multiply with it rather than replace it — the old 'attempts: x 3'
// problem, from when openai-go retried every request twice more of its own
// accord and this transport had to switch that off per response:
//
//	requests per failing turn  =  attempts:      (was: attempts: x 3)
//
// What that client did right is kept here: the provider's Retry-After and
// x-should-retry are honored over the status code.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// retryBackoffUnit is the linear pause between request retries, matching
// internal/retry's own coarse unit: these wrap a network round trip to an LLM
// endpoint, where a short pause is more likely to help than to waste time.
const retryBackoffUnit = 500 * time.Millisecond

// shouldRetryHeader is OpenAI's word on a response, above its status: "true" retries a 4xx, "false" does not retry a 5xx.
const shouldRetryHeader = "x-should-retry"

// retryAfterLimit is the longest a provider's Retry-After is waited out, openai-go's own bound: past it the attempt is better spent on fallback:.
const retryAfterLimit = 2 * time.Minute

// retryableStatus is what openai-go retried: request timeout, conflict, rate
// limit, and every 5xx.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	default:
		return code >= http.StatusInternalServerError
	}
}

// RequestCounter tallies the provider requests one agent conversation makes.
// It is what lets a failed step report the requests it really spent rather
// than the turns it took.
type RequestCounter struct {
	mu    sync.Mutex
	total int
}

// record counts one round trip.
func (c *RequestCounter) record() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.total++
}

// Total is the number of provider requests counted so far.
func (c *RequestCounter) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.total
}

type requestCounterKey struct{}

// WithRequestCounter scopes a counter to one agent conversation, so the count
// a failure reports describes that conversation alone.
func WithRequestCounter(ctx context.Context, counter *RequestCounter) context.Context {
	return context.WithValue(ctx, requestCounterKey{}, counter)
}

func requestCounterFrom(ctx context.Context) *RequestCounter {
	counter, _ := ctx.Value(requestCounterKey{}).(*RequestCounter)

	return counter
}

// requestRetryTransport is the agent step's attempts:. It retries one failing
// request up to attempts times — after the provider's Retry-After when it
// sent one, a linear backoff otherwise — and logs every retry. A transport,
// because it is the only layer that sees an individual request, which is the
// operation attempts: names.
type requestRetryTransport struct {
	base     http.RoundTripper
	agent    string
	model    string
	attempts int
}

func (t *requestRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.retryLoop(req)
}

// retryLoop sends req until it succeeds, fails unretryably, or runs out of
// attempts.
func (t *requestRetryTransport) retryLoop(req *http.Request) (*http.Response, error) {
	attempts := max(t.attempts, 1)

	for attempt := 1; ; attempt++ {
		resp, err := t.roundTripOnce(req)
		if !t.retryable(resp, err) || attempt >= attempts || !replayable(req) {
			return resp, err
		}

		delay, ok := retryDelay(resp, attempt)
		if !ok {
			return resp, err
		}

		t.logRetry(req.Context(), resp, err, attempt, attempts)
		discard(resp)

		select {
		case <-req.Context().Done():
			return nil, req.Context().Err() //nolint:wrapcheck // ctx.Err() is a well-known sentinel; wrapping adds nothing
		case <-time.After(delay):
		}

		req, err = rewind(req)
		if err != nil {
			return nil, err
		}
	}
}

// roundTripOnce performs one request and counts it.
func (t *requestRetryTransport) roundTripOnce(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)

	if counter := requestCounterFrom(req.Context()); counter != nil {
		counter.record()
	}

	return resp, err //nolint:wrapcheck // a RoundTripper must return the transport's error unwrapped
}

// retryable reports whether a result is worth another request: a connection
// error, or one of the statuses the provider may recover from.
func (t *requestRetryTransport) retryable(resp *http.Response, err error) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}

	//nolint:nilaway // err is nil here, and a RoundTripper returning a nil error must return a response
	switch resp.Header.Get(shouldRetryHeader) {
	case "true":
		return true
	case "false":
		return false
	}

	return retryableStatus(resp.StatusCode) //nolint:nilaway // err is nil here, and a RoundTripper returning a nil error must return a response
}

func (t *requestRetryTransport) logRetry(ctx context.Context, resp *http.Response, err error, attempt, attempts int) {
	fields := []any{
		"agent", t.agent,
		"model", t.model,
		"attempt", attempt,
		"attempts", attempts,
	}

	if err != nil {
		fields = append(fields, "error", err)
	} else {
		fields = append(fields, "status", resp.StatusCode) //nolint:nilaway // the else of err != nil: a RoundTripper returning a nil error must return a response
	}

	slog.WarnContext(ctx, "agent.request_retry", fields...)
}

// retryDelay is the wait before the retry after attempt: the provider's Retry-After-Ms, else its Retry-After (seconds or an HTTP date), else the linear backoff. ok is false when the provider asked for longer than retryAfterLimit.
func retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	if resp != nil {
		if delay, ok, found := retryAfter(resp.Header.Get("Retry-After-Ms"), time.Millisecond); found {
			return delay, ok
		}

		if delay, ok, found := retryAfter(resp.Header.Get("Retry-After"), time.Second); found {
			return delay, ok
		}
	}

	return time.Duration(attempt) * retryBackoffUnit, true
}

// retryAfter reads one header's value as a count of unit, or — for seconds — an HTTP date. found is false for a value that says nothing usable.
func retryAfter(value string, unit time.Duration) (time.Duration, bool, bool) {
	if value == "" {
		return 0, false, false
	}

	number, err := strconv.ParseFloat(value, 64)
	if err == nil {
		switch {
		case number < 0 || math.IsNaN(number):
			return 0, false, false
		case number > float64(retryAfterLimit/unit):
			return 0, false, true
		default:
			return time.Duration(number * float64(unit)), true, true
		}
	}

	when, err := http.ParseTime(value)
	if err != nil || unit != time.Second {
		return 0, false, false
	}

	delay := max(time.Until(when), 0)
	if delay > retryAfterLimit {
		return 0, false, true
	}

	return delay, true, true
}

// replayable reports whether a request's body can be sent again. A body with
// no GetBody has already been consumed, so retrying would send an empty one —
// the same rule net/http applies to its own retries.
func replayable(req *http.Request) bool {
	return req.Body == nil || req.GetBody != nil
}

// rewind returns a copy of req with a fresh body for the next attempt.
func rewind(req *http.Request) (*http.Request, error) {
	if req.Body == nil {
		return req, nil
	}

	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("rewind request body for retry: %w", err)
	}

	next := req.Clone(req.Context())
	next.Body = body

	return next, nil
}

// isTransientProviderError classifies an error that already came back out of
// a finished conversation attempt (attempts: exhausted, or a single
// non-retryable failure) as the kind fallback:'s mid-run cascade should react
// to — the same "connection-level failure" class the doc already promises:
// "a timeout, an unreachable endpoint, a 5xx". A model *refusing* a request
// (a 4xx) is a different class entirely and must not trigger a source swap.
//
// It classifies post-hoc rather than tagging the error where
// requestRetryTransport gives up, because a persistent 5xx never becomes a Go
// error at the transport layer at all — RoundTrip returns (resp, nil) for
// any completed HTTP exchange, and it is chatModel, one layer up, that turns a
// non-2xx status into *APIError after inspecting it. Classifying by
// status code on the way back out sees both shapes (a connection error that
// never got a response, and a response chatModel turned into an error)
// uniformly, without needing to plumb anything through the transport.
//
// The error reaching here is not necessarily a provider error at all: unlike
// (*requestRetryTransport).retryable, which only ever sees what a
// RoundTripper can return (a genuine network failure), this classifies
// whatever runOneConversation returned — which also includes this package's
// OWN internal errors (a budget breach, a malformed/empty response, a
// detected loop). None of those are "an unreachable endpoint" and must not
// trigger a source swap either, so each transient shape is recognized
// explicitly rather than defaulting to true for anything unrecognized.
//
// Neither context sentinel is transient here. A canceled run says nothing
// about the provider, and a spent deadline cannot be distinguished from work
// that was simply too big — ClassifySource handles both as sourceUnproven,
// which is what keeps either from moving an agent's pinned source.
func isTransientProviderError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return retryableStatus(apiErr.StatusCode)
	}

	if errors.Is(err, context.Canceled) {
		// A canceled run says nothing about the provider's health — the same
		// exclusion (*requestRetryTransport).retryable makes.
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		// A spent deadline is not evidence about the endpoint. It reads the
		// same whether the endpoint hung or the work was simply larger than
		// the budget allowed, and the cascade has no time left to find out:
		// timeout: bounds the STEP, so every remaining source would begin
		// already expired. ClassifySource calls this sourceUnproven and
		// changes nothing about which source the process prefers.
		return false
	}

	// A connection that dies partway through a response body: the exchange
	// began, so nothing produced an *APIError or a net.Error, and the
	// decoder surfaces whatever the truncated bytes decoded to. Reading a
	// half-delivered answer is a transport failure wearing a parser's
	// clothes.
	//
	// ErrUnexpectedEOF only, not a bare io.EOF: EOF is the stdlib's most
	// widely reused sentinel and the NORMAL terminator of any read, so
	// treating it as an outage lets an ordinary end-of-stream anywhere in
	// this package cascade to another model. A truncated JSON decode is
	// exactly what returns ErrUnexpectedEOF instead.
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, new(net.Error))
}

// discard drains and closes a response being thrown away, so its connection
// returns to the pool instead of being torn down on every retry.
func discard(resp *http.Response) {
	if resp == nil {
		return
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}
