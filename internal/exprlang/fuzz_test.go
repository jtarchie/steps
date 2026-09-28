package exprlang

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// FuzzCompile: no source panics the compiler, and the in slot sees everything check does, so a check that compiles compiles as an in too.
func FuzzCompile(f *testing.F) {
	f.Add(`[{"ts": version.ts ?? "0"}]`)
	f.Add(`http({"url": source.url}).json`)
	f.Add(`file("x") + params.y`)
	f.Add(`now()`)
	f.Add(`let v = version; v.x`)

	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 256 {
			return
		}

		checkErr := Compile(SlotCheck, src)
		inErr := Compile(SlotIn, src)
		_ = Compile(SlotOut, src)

		if checkErr == nil && inErr != nil {
			t.Fatalf("%q compiles as check but not as in: %v", src, inErr)
		}
	})
}

// FuzzWebhook: a webhook expression is pure, so the same delivery evaluates to the same result every time, and a filter that runs yields a bool.
func FuzzWebhook(f *testing.F) {
	f.Add(`payload.ref == "refs/heads/main"`, true, `{"ref":"refs/heads/main"}`, "push", "x-a")
	f.Add(`headers["x-a"] + event`, false, `[]`, "e", "x-a")
	f.Add(`body`, false, `not json`, "", "")
	f.Add(`len(payload) > 0`, true, `{"a":[1,2]}`, "", "")

	f.Fuzz(func(t *testing.T, src string, wantBool bool, body, event, header string) {
		if len(src) > 128 || len(body) > 1024 {
			return
		}

		run, err := Webhook(src, wantBool)
		if err != nil {
			return
		}

		var payload any

		_ = json.Unmarshal([]byte(body), &payload)

		env := WebhookEnv{
			Provider: "github",
			Event:    event,
			Method:   "POST",
			Headers:  map[string]string{strings.ToLower(header): body},
			Query:    map[string]string{"q": event},
			Body:     body,
			Payload:  payload,
		}

		first, firstErr := run(env)
		second, secondErr := run(env)

		if fmt.Sprintf("%#v %v", first, firstErr) != fmt.Sprintf("%#v %v", second, secondErr) {
			t.Fatalf("%q is not a pure function of the delivery: %#v (%v) then %#v (%v)", src, first, firstErr, second, secondErr)
		}

		if firstErr != nil || !wantBool {
			return
		}

		if _, ok := first.(bool); !ok {
			t.Fatalf("filter %q ran to %T, want bool", src, first)
		}
	})
}

// FuzzParseOptions: whatever a pipeline writes in http()'s settings, an accepted one is inside the bounds that keep a poll from exhausting the machine.
//
//nolint:cyclop // one branch per property the fuzzed input is held to
func FuzzParseOptions(f *testing.F) {
	f.Add(`{"concurrency": 1e300, "max_response_bytes": -5, "retry": {"on": [429], "max": 99}}`)
	f.Add(`{"concurrency": 8, "timeout": "30s", "headers": {"a": 1.5, "b": null}, "tolerate_errors": true}`)
	f.Add(`{"concurrency": "8"}`)
	f.Add(`{"retry": {"on": [-1e20], "max": -3}}`)

	f.Fuzz(func(t *testing.T, raw string) {
		var settings any

		err := json.Unmarshal([]byte(raw), &settings)
		if err != nil {
			return
		}

		options, err := parseOptions(nil, settings)
		if err != nil {
			return
		}

		if options.concurrency < 1 || options.concurrency > maxConcurrency {
			t.Fatalf("%s: concurrency %d outside [1, %d]", raw, options.concurrency, maxConcurrency)
		}

		if options.maxBytes < 1 || options.maxBytes > math.MaxInt32 {
			t.Fatalf("%s: max_response_bytes %d outside [1, MaxInt32]", raw, options.maxBytes)
		}

		if options.retryMax < 0 || options.retryMax > 10 {
			t.Fatalf("%s: retry.max %d outside [0, 10]", raw, options.retryMax)
		}

		for _, code := range options.retryOn {
			if code < 0 {
				t.Fatalf("%s: retry.on holds %d", raw, code)
			}
		}
	})
}

// FuzzParseRequests: an accepted request has a url, a canonical method, a body only when it was given one, and every query value it was asked to send.
//
//nolint:cyclop // one branch per property the fuzzed input is held to
func FuzzParseRequests(f *testing.F) {
	f.Add(`{"url": "https://x.test/a?b=1", "query": {"page": 2, "b": "z"}}`)
	f.Add(`[{"url": "u", "json": {"a": 1}}, {"url": "v", "body": "text", "method": "put"}]`)
	f.Add(`{"url": "u", "json": 1, "body": "x"}`)
	f.Add(`{"url": "%zz", "query": {"a": 1}}`)

	f.Fuzz(func(t *testing.T, raw string) {
		var value any

		err := json.Unmarshal([]byte(raw), &value)
		if err != nil {
			return
		}

		requests, single, err := parseRequests(value)
		if err != nil {
			return
		}

		items, isList := value.([]any)
		if single == isList || (isList && len(items) != len(requests)) {
			t.Fatalf("%s: single=%v for %d requests from %T", raw, single, len(requests), value)
		}

		for _, request := range requests {
			if request.url == "" {
				t.Fatalf("%s: accepted a request with no url", raw)
			}

			if request.method != strings.ToUpper(request.method) {
				t.Fatalf("%s: method %q is not canonical", raw, request.method)
			}

			_, hasJSON := request.raw["json"]
			_, hasBody := request.raw["body"]

			if hasJSON && hasBody {
				t.Fatalf("%s: accepted both json: and body:", raw)
			}

			if request.body != nil && !hasJSON && !hasBody {
				t.Fatalf("%s: a body nobody asked for", raw)
			}

			if _, set := request.raw["method"]; !set && (hasJSON || hasBody) && request.method != "POST" {
				t.Fatalf("%s: a payload defaulted to %s, want POST", raw, request.method)
			}
		}
	})
}

// FuzzNormalizeNumbers: no json.Number survives into what expr computes with, and an integer keeps its exact value.
//
//nolint:cyclop // one branch per property the fuzzed input is held to
func FuzzNormalizeNumbers(f *testing.F) {
	f.Add(`{"id": 1234567890123456789, "price": 1.5, "list": [1e400, -0, 3]}`)
	f.Add(`[99999999999999999999, {"a": [[1]]}]`)

	f.Fuzz(func(t *testing.T, raw string) {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()

		var value any

		err := decoder.Decode(&value)
		if err != nil {
			return
		}

		var walk func(before, after any)

		walk = func(before, after any) {
			switch typed := before.(type) {
			case json.Number:
				whole, err := strconv.ParseInt(typed.String(), 10, 64)

				switch got := after.(type) {
				case json.Number:
					t.Fatalf("%s: json.Number %s survived", raw, typed)
				case int64:
					if err != nil || got != whole {
						t.Fatalf("%s: %s became int64 %d", raw, typed, got)
					}
				case float64, string:
					if err == nil {
						t.Fatalf("%s: integer %s became %T", raw, typed, got)
					}
				default:
					t.Fatalf("%s: %s became %T", raw, typed, got)
				}
			case map[string]any:
				for key, item := range typed {
					walk(item, after.(map[string]any)[key]) //nolint:forcetypeassert // normalizeNumbers keeps containers
				}
			case []any:
				for i, item := range typed {
					walk(item, after.([]any)[i]) //nolint:forcetypeassert // as above
				}
			}
		}

		before := cloneJSON(value)
		walk(before, normalizeNumbers(value))
	})
}

func cloneJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = cloneJSON(item)
		}

		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneJSON(item)
		}

		return out
	default:
		return value
	}
}

// FuzzRetryDelay: whatever a server says in Retry-After, the wait is never negative (a hot loop) and never past the cap (a hung poll).
func FuzzRetryDelay(f *testing.F) {
	f.Add("120", uint8(0))
	f.Add("100000000000", uint8(3))
	f.Add("-1", uint8(9))
	f.Add("Wed, 21 Oct 2015 07:28:00 GMT", uint8(1))
	f.Add("", uint8(10))

	f.Fuzz(func(t *testing.T, after string, attempt uint8) {
		// doWithRetry calls this only below retry.max, which parseOptions clamps to 10.
		attempt %= 11

		got := retryDelay(map[string]any{"headers": map[string]string{"Retry-After": after}}, int(attempt))
		if got < 0 || got > maxRetryAfter {
			t.Fatalf("Retry-After %q, attempt %d: waited %s", after, attempt, got)
		}

		seconds, err := strconv.Atoi(after)
		if err == nil && seconds >= 0 {
			if want := min(time.Duration(min(seconds, 3600))*time.Second, maxRetryAfter); got != want {
				t.Fatalf("Retry-After %q: waited %s, want %s", after, got, want)
			}
		}
	})
}
