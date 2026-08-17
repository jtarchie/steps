package agent

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
// honest. The LLM client retries every request twice on its own (openai-go/v3
// defaults to MaxRetries: 2) and adk-utils-go's Config exposes no knob for it,
// so a retry loop anywhere above this transport MULTIPLIES with that hidden
// one rather than replacing it — the old 'attempts: x 3' problem. Owning the
// retry at the transport lets us also switch the SDK's own off per response,
// so attempts: is the whole story:
//
//	requests per failing turn  =  attempts:      (was: attempts: x 3)

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
)

// retryBackoffUnit is the linear pause between request retries, matching
// internal/retry's own coarse unit: these wrap a network round trip to an LLM
// endpoint, where a short pause is more likely to help than to waste time.
const retryBackoffUnit = 500 * time.Millisecond

// The two headers this transport uses to take ownership of retrying from the
// SDK. Both are part of openai-go's documented behavior, not a workaround:
//
//   - shouldRetryHeader on a RESPONSE tells the client not to retry it
//     (requestconfig.shouldRetry honors it above the status code).
//   - retryCountHeader on a REQUEST is the client's own retry counter. A
//     non-zero value means the client is retrying something this transport has
//     already finished with, which only happens on the connection-error path
//     where no response exists to carry the header above.
const (
	shouldRetryHeader = "x-should-retry"
	retryCountHeader  = "X-Stainless-Retry-Count"
)

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

// requestCounter tallies the provider requests one agent conversation makes,
// and remembers the last connection error so the client's own retry of it
// costs nothing (see RoundTrip). It is what lets a failed step report the
// requests it really spent rather than the turns it took.
type requestCounter struct {
	mu        sync.Mutex
	total     int
	exhausted error
}

// record counts one round trip.
func (c *requestCounter) record() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.total++
}

// Total is the number of provider requests counted so far.
func (c *requestCounter) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.total
}

// setExhausted/takeExhausted carry a connection error across the client's own
// retry rounds. A response can refuse further retries with a header; an error
// cannot, because there is no response to put one on.
func (c *requestCounter) setExhausted(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.exhausted = err
}

func (c *requestCounter) takeExhausted() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.exhausted
}

type requestCounterKey struct{}

// withRequestCounter scopes a counter to one agent conversation, so the count
// a failure reports describes that conversation alone.
func withRequestCounter(ctx context.Context, counter *requestCounter) context.Context {
	return context.WithValue(ctx, requestCounterKey{}, counter)
}

func requestCounterFrom(ctx context.Context) *requestCounter {
	counter, _ := ctx.Value(requestCounterKey{}).(*requestCounter)

	return counter
}

// requestRetryTransport is the agent step's attempts:. It retries one failing
// request up to attempts times with a linear backoff, logs every retry, and
// tells the SDK not to add retries of its own on top.
//
// It has to be a transport rather than a wrapper around the conversation for
// two reasons. It is the only layer that can see an individual request, which
// is the operation attempts: now names; and it is the only layer that can
// switch off the client's built-in retry, without which any retry above it
// would multiply by three rather than replace.
type requestRetryTransport struct {
	base     http.RoundTripper
	agent    string
	model    string
	attempts int
}

func (t *requestRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The client is retrying a connection error this transport already spent
	// its attempts on. Hand back the same error without touching the network:
	// its retry budget is not ours to spend, and letting it through would
	// restore the multiplication this design removes.
	spent := t.alreadySpent(req)
	if spent != nil {
		return nil, spent
	}

	resp, err := t.retryLoop(req)

	// attempts: is the whole retry budget. Refuse the client's own rounds:
	// on a response with the header it honors, and on a connection error by
	// stashing it for the branch above, since there is no response to put a
	// header on.
	counter := requestCounterFrom(req.Context())

	switch {
	case resp != nil:
		resp.Header.Set(shouldRetryHeader, "false")
	case counter != nil:
		counter.setExhausted(err)
	}

	return resp, err
}

// alreadySpent reports the error a previous RoundTrip exhausted its attempts
// on, when this request is the client retrying that same failure.
func (t *requestRetryTransport) alreadySpent(req *http.Request) error {
	counter := requestCounterFrom(req.Context())
	if counter == nil || !isClientRetry(req) {
		return nil
	}

	return counter.takeExhausted()
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

		t.logRetry(resp, err, attempt, attempts)
		discard(resp)

		select {
		case <-req.Context().Done():
			return nil, req.Context().Err() //nolint:wrapcheck // ctx.Err() is a well-known sentinel; wrapping adds nothing
		case <-time.After(time.Duration(attempt) * retryBackoffUnit):
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

	if resp.Header.Get(shouldRetryHeader) == "false" {
		return false
	}

	return retryableStatus(resp.StatusCode)
}

func (t *requestRetryTransport) logRetry(resp *http.Response, err error, attempt, attempts int) {
	fields := []any{
		"agent", t.agent,
		"model", t.model,
		"attempt", attempt,
		"attempts", attempts,
	}

	if err != nil {
		fields = append(fields, "error", err)
	} else {
		fields = append(fields, "status", resp.StatusCode)
	}

	slog.Warn("agent.request_retry", fields...)
}

// isClientRetry reports whether the SDK is reissuing a request it already sent
// once. Its retry counter starts at 0 and increments per round, so anything
// above 0 is a retry.
func isClientRetry(req *http.Request) bool {
	count, err := strconv.Atoi(req.Header.Get(retryCountHeader))

	return err == nil && count > 0
}

// replayable reports whether a request's body can be sent again. A body with
// no GetBody has already been consumed, so retrying would send an empty one —
// the same rule the SDK applies to its own retries.
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
// any completed HTTP exchange, and it is the SDK, one layer up, that turns a
// non-2xx status into *openai.Error after inspecting it. Classifying by
// status code on the way back out sees both shapes (a connection error that
// never got a response, and a response the SDK turned into an error)
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
// that was simply too big — classifySource handles both as sourceUnproven,
// which is what keeps either from moving an agent's pinned source.
func isTransientProviderError(err error) bool {
	var apiErr *openai.Error
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
		// already expired. classifySource calls this sourceUnproven and
		// changes nothing about which source the process prefers.
		return false
	}

	// A connection that dies partway through a response body: the exchange
	// began, so nothing produced an *openai.Error or a net.Error, and the
	// SDK surfaces whatever the truncated bytes decoded to. Reading a
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
