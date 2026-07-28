// repair.go fixes malformed tool-call arguments at the HTTP response
// boundary, the last place the model's raw arguments text still exists.
//
// The genaiopenai adapter parses each tool call's `arguments` string into
// map[string]any and — on any parse failure — silently substitutes an EMPTY
// map (see parseJSONArgs in adk-utils-go). So a local model that emits
// truncated JSON (hit max_tokens mid-string), trailing commas, or prose
// wrapped around the object arrives at runAgentConversation as a call with
// NO arguments at all: its actual arguments are discarded, and the tool's
// "missing required argument" error gives the model no idea what happened.
// Weak local models (the lmstudio/ollama class) produce this often enough
// that a coding agent can burn most of its turn budget rediscovering the
// failure.
//
// The repair shape is copied from crush/fantasy's validateAndRepairToolCall
// (validate → repair → re-validate → else leave alone), moved to the
// transport because that is where the raw text is still available. The
// repair itself is deliberately minimal — it handles the failures local
// models actually produce (truncation, trailing commas, markdown fences /
// surrounding prose) and gives up on everything else. A body with nothing
// repairable passes through byte-identically, so the adapter's behavior for
// genuinely unparseable arguments is exactly what it was before this file
// existed: repair can only recover a call, never alter a valid one.
//
// Scope: installed on every agent LLM client (see agentHTTPClient in
// provider.go), since malformed arguments are an OpenAI-compat-wide hazard,
// not a provider-specific one.

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// agentResponseHeaderTimeout mirrors the bound openai-go's own
// defaultHTTPClient applies when the caller supplies no client. Supplying
// one (which every agent now does, for the repair transport) opts out of
// that default, so it is re-applied here rather than silently dropping
// stuck-connection protection.
const agentResponseHeaderTimeout = 10 * time.Minute

// maxRepairResponseBytes caps how large a chat-completion response body the
// repair transport buffers. Responses are a few KB of choices in practice;
// the cap keeps a pathological server from turning an opportunistic repair
// pass into an unbounded allocation. Larger bodies pass through unrepaired.
const maxRepairResponseBytes = 32 << 20

// repairTransport wraps a base RoundTripper and repairs malformed tool-call
// arguments in chat-completion RESPONSES (requests pass through untouched —
// see openRouterTransport for the request-mutating analog).
type repairTransport struct {
	base http.RoundTripper
}

// CloseIdleConnections forwards to the wrapped transport, so an
// http.Client holding this wrapper can still release its sockets — the same
// type-assertion contract openRouterTransport.CloseIdleConnections
// documents.
func (t *repairTransport) CloseIdleConnections() {
	closer, ok := t.base.(interface{ CloseIdleConnections() })
	if ok {
		closer.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper.
func (t *repairTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req)
	if err != nil {
		return resp, err //nolint:wrapcheck // surface the base transport's error verbatim, per the RoundTripper contract
	}

	if req.URL == nil || !strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
		return resp, nil
	}

	// Only a plain JSON response can hold tool_calls worth repairing. This
	// client never requests a stream, but checking Content-Type (rather than
	// assuming) keeps a text/event-stream body from being buffered whole.
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return resp, nil
	}

	body, readErr := readBodyCapped(resp.Body, maxRepairResponseBytes)
	if readErr != nil {
		// Repair is opportunistic and must never be the reason a request
		// fails: pass through what was recovered (possibly partial),
		// unrepaired. The adapter surfaces a short body as its own error.
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))

		return resp, nil //nolint:nilerr // deliberate: a body we couldn't fully read is passed through, not failed — see the comment above
	}

	repaired := repairChatCompletionBody(body)

	resp.Body = io.NopCloser(bytes.NewReader(repaired))
	resp.ContentLength = int64(len(repaired))

	return resp, nil
}

// readBodyCapped reads all of r (closing it), returning the bytes read.
// More than limit bytes present is reported as an error alongside the first
// limit bytes, so the caller can still pass something through.
func readBodyCapped(r io.ReadCloser, limit int64) ([]byte, error) {
	defer func() { _ = r.Close() }()

	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return body, fmt.Errorf("read response body: %w", err)
	}

	if int64(len(body)) > limit {
		return body[:limit], errors.New("agent: response body too large to repair")
	}

	return body, nil
}

// repairChatCompletionBody walks choices[].message.tool_calls[].function
// .arguments in a chat-completion response body, repairing any arguments
// string that does not parse as a JSON object. Everything is decoded as
// json.RawMessage and only re-encoded when at least one repair was applied,
// so an all-valid body is returned byte-identically (same splice discipline
// as withCacheControl in openrouter.go).
func repairChatCompletionBody(body []byte) []byte {
	var doc map[string]json.RawMessage

	err := json.Unmarshal(body, &doc)
	if err != nil {
		return body
	}

	var choices []map[string]json.RawMessage

	err = json.Unmarshal(doc["choices"], &choices)
	if err != nil {
		return body
	}

	repairedAny := false

	for _, choice := range choices {
		if repairChoiceToolCalls(choice) {
			repairedAny = true
		}
	}

	if !repairedAny {
		return body
	}

	choicesRaw, err := json.Marshal(choices)
	if err != nil {
		return body
	}

	doc["choices"] = choicesRaw

	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	err = encoder.Encode(doc)
	if err != nil {
		return body
	}

	return bytes.TrimRight(buf.Bytes(), "\n")
}

// repairChoiceToolCalls repairs the arguments of every tool call in one
// choice's message object, writing the re-encoded message back into the
// choice map and reporting whether anything changed.
func repairChoiceToolCalls(choice map[string]json.RawMessage) bool {
	var msg map[string]json.RawMessage

	err := json.Unmarshal(choice["message"], &msg)
	if err != nil {
		return false
	}

	var calls []map[string]json.RawMessage

	err = json.Unmarshal(msg["tool_calls"], &calls)
	if err != nil {
		return false
	}

	repaired := false

	for _, call := range calls {
		if repairFunctionArguments(call) {
			repaired = true
		}
	}

	if !repaired {
		return false
	}

	callsRaw, err := json.Marshal(calls)
	if err != nil {
		return false
	}

	msg["tool_calls"] = callsRaw

	msgRaw, err := json.Marshal(msg)
	if err != nil {
		return false
	}

	choice["message"] = msgRaw

	return true
}

// repairFunctionArguments repairs one tool call's function.arguments string
// in place (through the call map), reporting whether it changed.
func repairFunctionArguments(call map[string]json.RawMessage) bool {
	var function map[string]json.RawMessage

	err := json.Unmarshal(call["function"], &function)
	if err != nil {
		return false
	}

	var args string

	err = json.Unmarshal(function["arguments"], &args)
	if err != nil {
		return false
	}

	fixed, changed := repairJSONArgs(args)
	if !changed {
		return false
	}

	raw, err := json.Marshal(fixed)
	if err != nil {
		return false
	}

	function["arguments"] = raw

	fnRaw, err := json.Marshal(function)
	if err != nil {
		return false
	}

	call["function"] = fnRaw

	return true
}

// repairJSONArgs attempts to repair one tool call's arguments string so it
// parses as a JSON object, copying the shape of fantasy's RepairJSON
// (best-effort, original returned when unrepairable) with a deliberately
// minimal rule set covering the failures local models actually produce:
//
//  1. Leading/trailing noise: markdown fences or prose around the object
//     (everything before the first '{' is dropped; everything after the
//     closing brace that balances it is dropped).
//  2. Truncation: an unterminated string is closed, a dangling ':' gets a
//     null value, a dangling ',' is dropped, and unclosed '{'/'[' are
//     closed in order.
//  3. Trailing commas before '}' or ']'.
//
// It returns the original string and false when the input already parses,
// has no object to salvage, or still doesn't parse after repair — a
// semantic mismatch (single quotes, YAML-ish text) is out of scope by
// design.
func repairJSONArgs(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || validJSONObject(trimmed) {
		return raw, false
	}

	start := strings.IndexByte(trimmed, '{')
	if start < 0 {
		return raw, false
	}

	candidate, complete := scanJSONObject(trimmed[start:])
	if complete {
		if fixed, ok := tryParse(candidate); ok {
			// Compare against the ORIGINAL, not candidate: tryParse may
			// have comma-stripped candidate into something different even
			// when candidate == trimmed.
			return fixed, fixed != raw
		}
	}

	// Truncated (or only valid after comma cleanup): close it up and retry.
	closed := closeJSONObject(candidate)
	if fixed, ok := tryParse(closed); ok {
		return fixed, true
	}

	if fixed, ok := tryParse(stripTrailingCommas(closed)); ok {
		return fixed, true
	}

	return raw, false
}

// validJSONObject reports whether s parses as JSON at all (the caller only
// feeds strings the adapter already failed on, so this is a cheap guard
// against doing work — or claiming a change — on an already-valid string).
func validJSONObject(s string) bool {
	var v any

	return json.Unmarshal([]byte(s), &v) == nil
}

// tryParse returns s (or its comma-stripped form first, when that parses)
// if it parses as JSON.
func tryParse(s string) (string, bool) {
	if validJSONObject(s) {
		return s, true
	}

	stripped := stripTrailingCommas(s)
	if stripped != s && validJSONObject(stripped) {
		return stripped, true
	}

	return "", false
}

// jsonLexState tracks the two things a single pass over JSON-ish text needs
// to know at every byte: whether we're inside a string (and escaped), and
// which '{'/'[' openers are still unclosed. Shared by scanJSONObject,
// closeJSONObject, and stripTrailingCommas so the string-state machine
// exists exactly once.
type jsonLexState struct {
	inString bool
	escaped  bool
	stack    []byte
}

// advance consumes one byte. It reports whether a '}'/']' just emptied the
// opener stack — meaningful only to the complete-value scan.
func (s *jsonLexState) advance(c byte) (emptied bool) {
	if s.inString {
		s.advanceInString(c)

		return false
	}

	switch c {
	case '"':
		s.inString = true
	case '{', '[':
		s.stack = append(s.stack, c)
	case '}', ']':
		if len(s.stack) > 0 {
			s.stack = s.stack[:len(s.stack)-1]
		}

		return len(s.stack) == 0
	}

	return false
}

// advanceInString consumes one byte while inside a string literal: only an
// unescaped '"' ends the string.
func (s *jsonLexState) advanceInString(c byte) {
	switch {
	case s.escaped:
		s.escaped = false
	case c == '\\':
		s.escaped = true
	case c == '"':
		s.inString = false
	}
}

// scanJSONObject scans s from its first byte (which must be '{') and
// returns the longest prefix that ends at the brace balancing that first
// '{' — or the whole string when no balancing brace exists (the truncated
// case, reported as complete == false).
func scanJSONObject(s string) (string, bool) {
	var state jsonLexState

	for i := range len(s) {
		if state.advance(s[i]) {
			return s[:i+1], true
		}
	}

	return s, false
}

// closeJSONObject repairs a truncated JSON fragment: closes an unterminated
// string, supplies null for a dangling key, drops a dangling comma, then
// closes every unclosed '{'/'[' in reverse order of opening.
func closeJSONObject(s string) string {
	var state jsonLexState

	for i := range len(s) {
		state.advance(s[i])
	}

	var out strings.Builder

	out.Grow(len(s) + len(state.stack) + 6)
	out.WriteString(s)

	if state.inString {
		out.WriteByte('"')
	}

	// A dangling colon means the value never arrived — supply null so the
	// key survives (a missing value the model can re-emit beats a dropped
	// key). A dangling comma is simply dropped.
	trimmed := strings.TrimRight(out.String(), " \t\r\n")

	for strings.HasSuffix(trimmed, ",") || strings.HasSuffix(trimmed, ":") {
		if strings.HasSuffix(trimmed, ":") {
			trimmed += " null"

			break
		}

		trimmed = strings.TrimRight(strings.TrimSuffix(trimmed, ","), " \t\r\n")
	}

	out.Reset()
	out.WriteString(trimmed)

	for i := len(state.stack) - 1; i >= 0; i-- {
		if state.stack[i] == '{' {
			out.WriteByte('}')
		} else {
			out.WriteByte(']')
		}
	}

	return out.String()
}

// stripTrailingCommas removes commas that immediately precede a '}' or ']'
// (mod whitespace), tracking string state so a comma inside a string value
// is never touched.
func stripTrailingCommas(s string) string {
	var (
		out   strings.Builder
		state jsonLexState
	)

	out.Grow(len(s))

	for i := range len(s) {
		c := s[i]
		wasInString := state.inString

		state.advance(c)

		switch {
		case wasInString || state.inString:
			out.WriteByte(c)
		case c == ',':
			if nextNonSpaceIsCloser(s, i) {
				continue // drop the comma
			}

			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}

	return out.String()
}

// nextNonSpaceIsCloser reports whether the next non-whitespace byte after
// s[i] is a '}' or ']'.
func nextNonSpaceIsCloser(s string, i int) bool {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case ' ', '\t', '\r', '\n':
			continue
		case '}', ']':
			return true
		default:
			return false
		}
	}

	return false
}

// agentBaseTransport is the transport every agent LLM client shares, built
// once. It reproduces openai-go's defaultHTTPClient (clone
// http.DefaultTransport, bound the wait for response headers) and is
// process-wide rather than per-client: only the thin per-agent wrappers
// (repair, and for OpenRouter the session/cache transport) differ per
// agent, so every agent step, sub-agent, and fix agent reuses one
// connection pool instead of paying a fresh TLS handshake each.
//
// Cloning once (rather than using http.DefaultTransport directly) keeps the
// response-header bound off every other HTTP user in the process
// (internal/mcp, resource types shelling out, ...).
var agentBaseTransport = sync.OnceValue(func() http.RoundTripper { //nolint:gochecknoglobals // process-wide connection pool, built lazily on first agent
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}

	transport = transport.Clone()
	transport.ResponseHeaderTimeout = agentResponseHeaderTimeout

	return transport
})
