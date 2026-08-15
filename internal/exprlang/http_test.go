package exprlang

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// jsonServer answers every request with a JSON object echoing what it got, so
// a test can assert on method, path, query and headers from inside the
// expression rather than from Go.
func jsonServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}

		w.Header().Set("Content-Type", "application/json")

		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"method": r.Method,
			"path":   r.URL.Path,
			"query":  r.URL.Query().Get("channel"),
			"auth":   r.Header.Get("Authorization"),
			"body":   string(body),
		})
	}))

	t.Cleanup(server.Close)

	return server
}

func TestHTTPSingleRequestIsSugarForABatch(t *testing.T) {
	t.Parallel()

	server := jsonServer(t)

	// One request in, one envelope out — and it is still an ENVELOPE, not a
	// bare body, so nothing has to change when the call later becomes a batch.
	versions, err := RunCheck(context.Background(),
		`[{status: string(http({url: source.url}).status), ok: string(http({url: source.url}).json.ok)}]`,
		Input{Source: map[string]any{"url": server.URL}})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if versions[0]["status"] != "200" || versions[0]["ok"] != "true" {
		t.Fatalf("versions = %+v", versions)
	}
}

func TestHTTPBatchKeepsRequestOrderAndAttribution(t *testing.T) {
	t.Parallel()

	server := jsonServer(t)

	// The envelope carries the request that produced it, which is what lets
	// an expression recover which channel a response belongs to without
	// zipping two arrays by index — and what makes an error message able to
	// say "channel C3" instead of "request 3".
	src := fmt.Sprintf(`
	  let reqs = ["c1", "c2", "c3"] | map((
	    {url: %q, query: {channel: #}}
	  ));
	  http(reqs, {concurrency: 3})
	    | map((
	      {channel: #.request.query.channel, echoed: #.json.query}
	    ))
	`, server.URL)

	versions, err := RunCheck(context.Background(), src, Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	want := []string{"c1", "c2", "c3"}
	if len(versions) != len(want) {
		t.Fatalf("versions = %+v", versions)
	}

	for i, channel := range want {
		if versions[i]["channel"] != channel || versions[i]["echoed"] != channel {
			t.Errorf("version %d = %+v, want channel %q — results must come back in REQUEST order", i, versions[i], channel)
		}
	}
}

// TestHTTPBatchIsActuallyConcurrent is the claim worth proving rather than
// asserting: the batch exists because a serial 1+N check is the thing shell
// cannot fix. The handler blocks until it sees `concurrency` requests at
// once, so a serial implementation deadlocks and fails the test by timeout
// instead of passing quietly.
func TestHTTPBatchIsActuallyConcurrent(t *testing.T) {
	t.Parallel()

	const concurrency = 4

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)

	gate := make(chan struct{})

	var released sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++

		if inFlight > peak {
			peak = inFlight
		}

		reached := inFlight >= concurrency
		mu.Unlock()

		if reached {
			released.Do(func() { close(gate) })
		}

		select {
		case <-gate:
		case <-time.After(5 * time.Second):
		}

		mu.Lock()
		inFlight--
		mu.Unlock()

		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	src := fmt.Sprintf(`
	  let reqs = 1..8 | map((
	    {url: %q}
	  ));
	  http(reqs, {concurrency: %d}) | map((
	    {status: string(#.status)}
	  ))
	`, server.URL, concurrency)

	versions, err := RunCheck(context.Background(), src, Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if len(versions) != 8 {
		t.Fatalf("got %d versions, want 8", len(versions))
	}

	mu.Lock()
	defer mu.Unlock()

	if peak < concurrency {
		t.Errorf("peak in-flight requests = %d, want at least %d", peak, concurrency)
	}

	if peak > concurrency {
		t.Errorf("peak in-flight requests = %d, want at most the configured %d", peak, concurrency)
	}
}

func TestHTTPSharedHeadersMergeAndRequestWins(t *testing.T) {
	t.Parallel()

	server := jsonServer(t)

	// Shared settings are the second argument precisely because expr has no
	// merge(): the API is shaped so callers never need one.
	src := fmt.Sprintf(`
	  let results = http([
	    {url: %[1]q},
	    {url: %[1]q, headers: {Authorization: "Bearer own"}},
	  ], {headers: {Authorization: "Bearer shared"}});
	  results | map((
	    {auth: #.json.auth}
	  ))
	`, server.URL)

	versions, err := RunCheck(context.Background(), src, Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if versions[0]["auth"] != "Bearer shared" {
		t.Errorf("first auth = %v, want the shared header", versions[0]["auth"])
	}

	if versions[1]["auth"] != "Bearer own" {
		t.Errorf("second auth = %v, want the request's own header to win", versions[1]["auth"])
	}
}

func TestHTTPPostWithJSONBody(t *testing.T) {
	t.Parallel()

	server := jsonServer(t)

	// json: implies POST and sets the content type: a request carrying a
	// payload is not a GET, and saying so twice is noise.
	versions, err := RunCheck(context.Background(), fmt.Sprintf(`
	  let posted = http({url: %q, json: {text: "hi"}});
	  [{method: posted.json.method, body: posted.json.body}]
	`, server.URL), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if versions[0]["method"] != "POST" || versions[0]["body"] != `{"text":"hi"}` {
		t.Fatalf("versions = %+v", versions)
	}
}

// TestHTTPRetriesOnListedStatusHonoringRetryAfter covers the reason retry
// belongs inside the batch: a rate limit is a property of the batch, and
// nobody writes Retry-After handling by hand.
func TestHTTPRetriesOnListedStatusHonoringRetryAfter(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	versions, err := RunCheck(context.Background(), fmt.Sprintf(`
	  let r = http({url: %q}, {retry: {on: [429, 503], max: 3}});
	  [{status: string(r.status)}]
	`, server.URL), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if versions[0]["status"] != "200" {
		t.Fatalf("status = %v after retries, want 200", versions[0]["status"])
	}

	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (two 429s then the success)", got)
	}
}

// TestHTTPExhaustedRetryReturnsLastResponse: a status is DATA. A persistent
// 429 is something the expression gets to decide about, not something that
// kills the poll from underneath it.
func TestHTTPExhaustedRetryReturnsLastResponse(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)

	versions, err := RunCheck(context.Background(), fmt.Sprintf(`
	  let r = http({url: %q}, {retry: {on: [429], max: 2}});
	  [{status: string(r.status)}]
	`, server.URL), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if versions[0]["status"] != "429" {
		t.Fatalf("status = %v, want the last response rather than an error", versions[0]["status"])
	}

	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (the original plus max: 2)", got)
	}
}

// TestHTTPNon2xxIsData: an API answering 200 with ok:false, or 404 for a
// thing that does not exist yet, is an answer. Only a request that never
// produced a response is a failure.
func TestHTTPNon2xxIsData(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_in_channel"}`))
	}))
	t.Cleanup(server.Close)

	versions, err := RunCheck(context.Background(), fmt.Sprintf(`
	  let r = http({url: %q});
	  [{status: string(r.status), why: r.json.error}]
	`, server.URL), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if versions[0]["status"] != "404" || versions[0]["why"] != "not_in_channel" {
		t.Fatalf("versions = %+v", versions)
	}
}

// TestHTTPTransportErrorFailsTheCall is the behavior that makes this worth
// leaving shell for: a dead request fails the check, loudly, instead of
// shrinking the version list and reading as "nothing new". The message names
// the request so the failure is attributable.
func TestHTTPTransportErrorFailsTheCall(t *testing.T) {
	t.Parallel()

	server := jsonServer(t)
	dead := server.URL
	server.Close()

	_, err := RunCheck(context.Background(), fmt.Sprintf(`http([{url: %q}]) | map((
	  {a: "b"}
	))`, dead), Input{})
	if err == nil {
		t.Fatal("RunCheck: want an error when a request cannot be made")
	}

	if !strings.Contains(err.Error(), "GET "+dead) {
		t.Errorf("err = %v, want the message to name the failing request", err)
	}
}

// TestHTTPTolerateErrors is the opt-in for partial tolerance: one channel a
// bot was removed from should not take out a poll over nineteen healthy
// ones. The failure becomes an envelope with an error field, filterable in
// the language, rather than being silently dropped.
func TestHTTPTolerateErrors(t *testing.T) {
	t.Parallel()

	server := jsonServer(t)
	good := server.URL
	dead := "http://127.0.0.1:1/nope"

	versions, err := RunCheck(context.Background(), fmt.Sprintf(`
	  http([{url: %q}, {url: %q}], {tolerate_errors: true, timeout: "2s"})
	    | filter(#.error == nil)
	    | map((
	      {status: string(#.status)}
	    ))
	`, good, dead), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if len(versions) != 1 || versions[0]["status"] != "200" {
		t.Fatalf("versions = %+v, want only the healthy request to survive the filter", versions)
	}
}

func TestHTTPTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}

		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	_, err := RunCheck(context.Background(), fmt.Sprintf(`http([{url: %q}], {timeout: "50ms"}) | map((
	  {a: "b"}
	))`, server.URL), Input{})
	if err == nil {
		t.Fatal("RunCheck: want a timeout error")
	}
}

// TestHTTPMaxResponseBytes: an expr backend holds a body in memory, so an
// endpoint answering with a gigabyte has to be a failure rather than an
// out-of-memory kill. Truncating instead would be worse — half a JSON
// document that happens to parse is a wrong answer that looks right.
func TestHTTPMaxResponseBytes(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	t.Cleanup(server.Close)

	_, err := RunCheck(context.Background(), fmt.Sprintf(`http([{url: %q}], {max_response_bytes: 100}) | map((
	  {a: "b"}
	))`, server.URL), Input{})
	if err == nil || !strings.Contains(err.Error(), "max_response_bytes") {
		t.Fatalf("err = %v, want a message naming the limit", err)
	}
}

// TestHTTPRejectsUnknownKeys: a misspelled `header:` that silently sent no
// authorization would surface as a 401 somewhere else entirely.
func TestHTTPRejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	_, err := RunCheck(context.Background(), `http({url: "http://x", header: {a: "b"}}) | map((
	  {a: "b"}
	))`, Input{})
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err = %v, want an error naming the unknown key", err)
	}

	_, err = RunCheck(context.Background(), `http({url: "http://x"}, {concurrncy: 2}) | map((
	  {a: "b"}
	))`, Input{})
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("err = %v, want an error naming the unknown setting", err)
	}
}

// TestHTTPPaginationByReduce pins the pattern that stands in for a
// paginate() builtin. Expr has no loops, but reduce threads an accumulator,
// so a cursor walk is a reduce over a BOUNDED range with an early-out — the
// page cap is the range itself, visible in the source, which auto-following
// a next_cursor would not be.
func TestHTTPPaginationByReduce(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"items":["a"],"next":"p2"}`))
		case "p2":
			_, _ = w.Write([]byte(`{"items":["b"],"next":"p3"}`))
		default:
			_, _ = w.Write([]byte(`{"items":["c"],"next":""}`))
		}
	}))
	t.Cleanup(server.Close)

	// Two things the docs must say out loud: without the #acc.done guard this
	// keeps calling http() past the end of the results, and lists are joined
	// with concat() — `+` is arithmetic here and refuses two arrays.
	src := fmt.Sprintf(`
	  reduce(1..10,
	    #acc.done ? #acc : (
	      let page = http({url: %q, query: {cursor: #acc.cursor}}).json;
	      let next = page.next ?? "";
	      {cursor: next, items: concat(#acc.items, page.items), done: next == ""}
	    ),
	    {cursor: "", items: [], done: false}
	  ).items | map((
	    {id: #}
	  ))
	`, server.URL)

	versions, err := RunCheck(context.Background(), src, Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if len(versions) != 3 || versions[0]["id"] != "a" || versions[2]["id"] != "c" {
		t.Fatalf("versions = %+v, want every page accumulated in order", versions)
	}
}
