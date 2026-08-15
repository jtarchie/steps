package exprlang

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// http() takes a LIST of requests, and a single request is sugar for a
// one-element list. Nearly all of this backend's leverage over shell lives in
// that one decision.
//
// A check against a chat API is 1 + N round trips — one to list the channels,
// one per channel — and in shell every one of them is serial. Expr cannot
// express concurrency at any price, and shell can only get it with background
// jobs and `wait`, which is precisely the construct that loses a command's
// exit status and produces the mktemp/trap dance this backend exists to
// delete. Batching is what makes concurrency safe to offer: steps owns the
// fan-out, so steps owns the error handling.
//
// A batch is also where rate limits happen, so a batch is where honoring
// Retry-After belongs. Nobody writes that correctly by hand, and the shell
// versions in the wild do not try.
//
// And each result carries the request that produced it, so a failure can name
// the channel rather than a line number, and a caller can recover which
// request a response belongs to without zipping two arrays by index.
const (
	defaultConcurrency = 4
	maxConcurrency     = 32
	defaultTimeout     = 30 * time.Second
	defaultMaxBytes    = 8 << 20 // 8 MiB
	maxRetryAfter      = 60 * time.Second
	maxRetryBackoff    = 30 * time.Second
)

// requestSpec is one parsed request. The raw map is kept verbatim for the
// response envelope: echoing back exactly what the caller wrote is what lets
// an expression recover `#.request.query.channel` for error attribution.
type requestSpec struct {
	raw     map[string]any
	method  string
	url     string
	headers map[string]string
	body    []byte
}

// httpOptions is the second argument: settings shared by every request in the
// batch. Shared headers live here rather than being merged into each request
// by the caller because expr has no merge() — so the API is shaped to make
// map merging unnecessary instead of making callers invent it.
type httpOptions struct {
	headers     map[string]string
	concurrency int
	timeout     time.Duration
	maxBytes    int64
	retryOn     []int
	retryMax    int
	tolerate    bool
}

// httpFunc builds the http() builtin bound to this evaluation's context.
func httpFunc(ctx context.Context) func(...any) (any, error) {
	return func(params ...any) (any, error) {
		if len(params) == 0 || len(params) > 2 {
			return nil, fmt.Errorf("http() takes a request (or a list of them) and optional settings, got %d arguments", len(params))
		}

		requests, single, err := parseRequests(params[0])
		if err != nil {
			return nil, err
		}

		options, err := parseOptions(params...)
		if err != nil {
			return nil, err
		}

		results, err := doBatch(ctx, requests, options)
		if err != nil {
			return nil, err
		}

		// Sugar in, sugar out: one request answers with one envelope. The
		// envelope itself is never skipped — .json rather than a bare parsed
		// body — so there is no special case to remember when a call later
		// grows into a batch.
		if single {
			return results[0], nil
		}

		out := make([]any, len(results))
		for i, result := range results {
			out[i] = result
		}

		return out, nil
	}
}

// parseRequests accepts a single request map or a list of them.
func parseRequests(value any) (requests []requestSpec, single bool, err error) {
	switch typed := value.(type) {
	case map[string]any:
		request, err := parseRequest(typed, 0)
		if err != nil {
			return nil, false, err
		}

		return []requestSpec{request}, true, nil
	case []any:
		requests := make([]requestSpec, 0, len(typed))

		for i, item := range typed {
			raw, ok := item.(map[string]any)
			if !ok {
				return nil, false, fmt.Errorf("http(): request %d is %T, want an object", i, item)
			}

			request, err := parseRequest(raw, i)
			if err != nil {
				return nil, false, err
			}

			requests = append(requests, request)
		}

		return requests, false, nil
	default:
		return nil, false, fmt.Errorf("http(): first argument is %T, want a request object or a list of them", value)
	}
}

// requestKeys and settingKeys are the complete vocabularies of the two maps
// http() accepts. Package-level so the check is one allocation for the whole
// program rather than one per key of every request in a batch.
var (
	requestKeys = []string{"url", "method", "query", "headers", "json", "body"}
	settingKeys = []string{"headers", "concurrency", "timeout", "max_response_bytes", "retry", "tolerate_errors"}
)

// parseRequest validates one request map. Unknown keys are rejected rather
// than ignored: a misspelled `header:` that silently sends no authorization
// fails as a 401 somewhere else entirely.
func parseRequest(raw map[string]any, index int) (requestSpec, error) {
	for key := range raw {
		if !slices.Contains(requestKeys, key) {
			return requestSpec{}, fmt.Errorf("http(): request %d has unknown key %q, want %s",
				index, key, strings.Join(requestKeys, "/"))
		}
	}

	rawURL, ok := raw["url"].(string)
	if !ok || rawURL == "" {
		return requestSpec{}, fmt.Errorf("http(): request %d has no url", index)
	}

	spec := requestSpec{raw: raw, url: rawURL, method: http.MethodGet}

	err := applyQuery(&spec, raw["query"], index)
	if err != nil {
		return requestSpec{}, err
	}

	spec.headers, err = stringMap(raw["headers"], fmt.Sprintf("http(): request %d headers", index))
	if err != nil {
		return requestSpec{}, err
	}

	err = applyBody(&spec, raw, index)
	if err != nil {
		return requestSpec{}, err
	}

	if method, present := raw["method"]; present {
		text, ok := method.(string)
		if !ok {
			return requestSpec{}, fmt.Errorf("http(): request %d method is %T, want a string", index, method)
		}

		spec.method = strings.ToUpper(text)
	}

	return spec, nil
}

// applyQuery folds a query: map into the URL. Values are stringified rather
// than required to be strings — a page number is a number to whoever wrote
// the expression, and url.Values holds strings either way.
func applyQuery(spec *requestSpec, query any, index int) error {
	if query == nil {
		return nil
	}

	pairs, ok := query.(map[string]any)
	if !ok {
		return fmt.Errorf("http(): request %d query is %T, want an object", index, query)
	}

	parsed, err := url.Parse(spec.url)
	if err != nil {
		return fmt.Errorf("http(): request %d url %q: %w", index, spec.url, err)
	}

	values := parsed.Query()

	// Iteration order does not leak into the result: Set replaces rather than
	// appends, and url.Values.Encode sorts by key — so the URL a batch
	// produces is deterministic without sorting here first.
	for key, value := range pairs {
		values.Set(key, scalarString(value))
	}

	parsed.RawQuery = values.Encode()
	spec.url = parsed.String()

	return nil
}

// applyBody sets the request body from json: or body:, and defaults the
// method to POST when either is present — a request carrying a payload is
// almost never a GET, and saying so twice is noise.
func applyBody(spec *requestSpec, raw map[string]any, index int) error {
	payload, hasJSON := raw["json"]
	body, hasBody := raw["body"]

	if hasJSON && hasBody {
		return fmt.Errorf("http(): request %d sets both json: and body:, which are two spellings of the same thing", index)
	}

	switch {
	case hasJSON:
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("http(): request %d json: %w", index, err)
		}

		spec.body = encoded
		spec.method = http.MethodPost

		if spec.headers == nil {
			spec.headers = map[string]string{}
		}

		if _, set := spec.headers["Content-Type"]; !set {
			spec.headers["Content-Type"] = "application/json"
		}
	case hasBody:
		text, ok := body.(string)
		if !ok {
			return fmt.Errorf("http(): request %d body is %T, want a string (use json: for an object)", index, body)
		}

		spec.body = []byte(text)
		spec.method = http.MethodPost
	}

	return nil
}

// parseOptions reads the optional second argument.
func parseOptions(params ...any) (httpOptions, error) {
	options := httpOptions{
		concurrency: defaultConcurrency,
		timeout:     defaultTimeout,
		maxBytes:    defaultMaxBytes,
	}

	if len(params) < 2 {
		return options, nil
	}

	settings, ok := params[1].(map[string]any)
	if !ok {
		return httpOptions{}, fmt.Errorf("http(): settings are %T, want an object", params[1])
	}

	for key := range settings {
		if !slices.Contains(settingKeys, key) {
			return httpOptions{}, fmt.Errorf("http(): unknown setting %q, want one of %s", key, strings.Join(settingKeys, "/"))
		}
	}

	var err error

	options.headers, err = stringMap(settings["headers"], "http(): settings headers")
	if err != nil {
		return httpOptions{}, err
	}

	err = applyBounds(&options, settings)
	if err != nil {
		return httpOptions{}, err
	}

	err = applyTimeout(&options, settings["timeout"])
	if err != nil {
		return httpOptions{}, err
	}

	err = applyRetry(&options, settings["retry"])
	if err != nil {
		return httpOptions{}, err
	}

	return options, nil
}

// applyBounds reads the settings that bound one batch's appetite:
// concurrency, response size, and whether a dead member sinks the batch.
func applyBounds(options *httpOptions, settings map[string]any) error {
	var err error

	options.concurrency, err = clampInt("concurrency", settings["concurrency"], defaultConcurrency, 1, maxConcurrency)
	if err != nil {
		return err
	}

	maxBytes, err := clampInt("max_response_bytes", settings["max_response_bytes"], defaultMaxBytes, 1, 1<<31)
	if err != nil {
		return err
	}

	options.maxBytes = int64(maxBytes)

	if tolerate, present := settings["tolerate_errors"]; present && tolerate != nil {
		flag, ok := tolerate.(bool)
		if !ok {
			return fmt.Errorf("http(): tolerate_errors is %T, want true or false", tolerate)
		}

		options.tolerate = flag
	}

	return nil
}

func applyTimeout(options *httpOptions, value any) error {
	if value == nil {
		return nil
	}

	switch typed := value.(type) {
	case string:
		parsed, err := time.ParseDuration(typed)
		if err != nil {
			return fmt.Errorf("http(): timeout %q: %w", typed, err)
		}

		options.timeout = parsed
	case time.Duration:
		options.timeout = typed
	default:
		return fmt.Errorf("http(): timeout is %T, want a duration string like \"30s\"", value)
	}

	return nil
}

// applyRetry reads retry: {on: [...], max: n}. Only the listed statuses are
// retried, and only that many times.
func applyRetry(options *httpOptions, value any) error {
	if value == nil {
		return nil
	}

	settings, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("http(): retry is %T, want an object like {on: [429], max: 3}", value)
	}

	statuses, ok := settings["on"].([]any)
	if !ok {
		return fmt.Errorf("http(): retry.on is %T, want a list of status codes", settings["on"])
	}

	for i, status := range statuses {
		code, err := clampInt(fmt.Sprintf("retry.on[%d]", i), status, 0, 0, 599)
		if err != nil {
			return err
		}

		options.retryOn = append(options.retryOn, code)
	}

	var err error

	options.retryMax, err = clampInt("retry.max", settings["max"], 0, 0, 10)
	if err != nil {
		return err
	}

	return nil
}

// doBatch runs every request, at most concurrency at a time, and returns the
// envelopes in REQUEST order — not completion order, which would make a
// result's meaning depend on the network.
func doBatch(ctx context.Context, requests []requestSpec, options httpOptions) ([]map[string]any, error) {
	results := make([]map[string]any, len(requests))
	errs := make([]error, len(requests))

	slots := make(chan struct{}, options.concurrency)
	done := make(chan int, len(requests))

	// The slot is taken BEFORE the goroutine is spawned, so concurrency: 4
	// over a thousand channels holds four goroutines rather than a thousand
	// of which 996 are parked on the semaphore. done is buffered to the full
	// batch, so no sender can block and the acquire below cannot deadlock.
	for i, request := range requests {
		slots <- struct{}{}

		go func() {
			defer func() { <-slots }()

			results[i], errs[i] = doWithRetry(ctx, request, options)
			done <- i
		}()
	}

	for range requests {
		<-done
	}

	for i, err := range errs {
		if err == nil {
			continue
		}

		// One failed request fails the call. Failing loudly is the whole
		// point of not writing this in shell, where a dead curl in a loop
		// reads as "nothing new". A batch that should survive a bad member
		// says so with tolerate_errors, and filters on #.error.
		if !options.tolerate {
			return nil, fmt.Errorf("http(): %s %s: %w", requests[i].method, requests[i].url, err)
		}

		results[i] = map[string]any{
			"request": requests[i].raw,
			"status":  0,
			"headers": map[string]string{},
			"json":    nil,
			"body":    "",
			"error":   err.Error(),
		}
	}

	return results, nil
}

// doWithRetry performs one request, retrying the statuses the caller listed.
//
// An exhausted retry returns the LAST response rather than an error: a status
// is data here (a chat API answers 200 with ok:false anyway), so an
// expression can decide what a persistent 429 means to it. Transport errors
// are not retried — a refused connection is usually a wrong URL, and retrying
// a wrong URL three times is just slower.
func doWithRetry(ctx context.Context, request requestSpec, options httpOptions) (map[string]any, error) {
	var envelope map[string]any

	for attempt := 0; ; attempt++ {
		result, err := doOnce(ctx, request, options)
		if err != nil {
			return nil, err
		}

		envelope = result

		status, _ := envelope["status"].(int)
		if attempt >= options.retryMax || !slices.Contains(options.retryOn, status) {
			return envelope, nil
		}

		wait := retryDelay(envelope, attempt)

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, ctx.Err() //nolint:wrapcheck // the caller names the request; the cause is the context's own
		case <-timer.C:
		}
	}
}

// retryDelay honors Retry-After when the server sent one — in seconds or as
// an HTTP date, both of which are legal — and otherwise backs off
// exponentially. Capped either way, so a server asking for an hour does not
// hang a poll.
func retryDelay(envelope map[string]any, attempt int) time.Duration {
	headers, _ := envelope["headers"].(map[string]string)

	if after := headers["Retry-After"]; after != "" {
		seconds, err := strconv.Atoi(after)
		if err == nil && seconds >= 0 {
			// Capped BEFORE the multiply, not after: a server asking for
			// 10^11 seconds overflows int64 nanoseconds into a negative
			// duration, and a negative timer fires at once — turning the cap
			// into a hot retry loop, which is the opposite of what it is for.
			if seconds > int(maxRetryAfter/time.Second) {
				return maxRetryAfter
			}

			return time.Duration(seconds) * time.Second
		}

		when, err := http.ParseTime(after)
		if err == nil {
			return min(max(time.Until(when), 0), maxRetryAfter)
		}
	}

	return min(time.Second<<attempt, maxRetryBackoff)
}

// doOnce performs a single round trip and builds its envelope.
func doOnce(ctx context.Context, request requestSpec, options httpOptions) (map[string]any, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()

	var body io.Reader
	if request.body != nil {
		body = bytes.NewReader(request.body)
	}

	req, err := http.NewRequestWithContext(attemptCtx, request.method, request.url, body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	for key, value := range options.headers {
		req.Header.Set(key, value)
	}

	// The request's own headers win over the batch's shared ones, which is
	// what makes {headers: auth} usable as a default rather than an override.
	for key, value := range request.headers {
		req.Header.Set(key, value)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err //nolint:wrapcheck // doBatch names the request; this is the cause
	}
	defer func() { _ = resp.Body.Close() }()

	// One byte past the limit is read on purpose: it is how "exactly at the
	// limit" is told from "truncated", and a truncated body parsed as JSON is
	// a worse outcome than a loud failure.
	data, err := io.ReadAll(io.LimitReader(resp.Body, options.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if int64(len(data)) > options.maxBytes {
		return nil, fmt.Errorf("response is larger than max_response_bytes (%d)", options.maxBytes)
	}

	return envelopeOf(request, resp, data), nil
}

// envelopeOf builds the uniform result shape: the request as written, the
// status, the headers, the parsed body, and the raw text.
//
// json is nil when the body does not parse, and body always holds the text —
// which is what makes a surprising response debuggable from inside the
// expression rather than only from a log.
func envelopeOf(request requestSpec, resp *http.Response, data []byte) map[string]any {
	headers := make(map[string]string, len(resp.Header))
	for key, values := range resp.Header {
		headers[key] = strings.Join(values, ", ")
	}

	var parsed any

	err := json.Unmarshal(data, &parsed)
	if err != nil {
		parsed = nil
	}

	return map[string]any{
		"request": request.raw,
		"status":  resp.StatusCode,
		"headers": headers,
		"json":    parsed,
		"body":    string(data),
		"error":   nil,
	}
}

// httpClient is shared so connections are reused across a batch and across
// the calls one expression makes. Redirects and TLS verification are the
// stdlib defaults, and there is deliberately no knob to weaken either.
var httpClient = &http.Client{}

// CloseIdleConnections releases the keep-alive connections a run left behind.
// Called when an evaluation finishes so a test's goroutine leak check sees a
// quiet package, and so a long watch loop does not hold sockets to an API it
// polls every few minutes.
func CloseIdleConnections() {
	httpClient.CloseIdleConnections()
}

// stringMap converts a header-ish map, stringifying scalar values so a
// numeric header does not need quoting at the call site.
func stringMap(value any, context string) (map[string]string, error) {
	if value == nil {
		// No headers is not an error and not a value worth allocating: every
		// caller ranges over the result, and ranging over a nil map is legal.
		return nil, nil //nolint:nilnil // "absent" is the whole meaning here
	}

	pairs, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is %T, want an object", context, value)
	}

	out := make(map[string]string, len(pairs))
	for key, item := range pairs {
		out[key] = scalarString(item)
	}

	return out, nil
}

// scalarString renders a query or header value. Floats print without an
// exponent so an id or a timestamp survives the trip.
func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

// clampInt reads an optional numeric setting, holding it inside a sane range
// rather than trusting a pipeline file with a value that could exhaust the
// machine.
//
// A value of the WRONG TYPE is an error rather than a silent fallback, for
// the same reason an unknown setting key is: `concurrency: "8"` quietly
// becoming 4, or `retry: {on: ["429"]}` quietly retrying nothing, is a
// setting that reads as applied and is not.
func clampInt(name string, value any, fallback, low, high int) (int, error) {
	var number int

	switch typed := value.(type) {
	case int:
		number = typed
	case int64:
		number = int(typed)
	case float64:
		number = int(typed)
	case nil:
		return fallback, nil
	default:
		return 0, fmt.Errorf("http(): %s is %T, want a number", name, value)
	}

	return min(max(number, low), high), nil
}
