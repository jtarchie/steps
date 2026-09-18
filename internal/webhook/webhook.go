// Package webhook turns one delivery into a version; expressions reach it compiled, so one can build a string but never decide that a signature is valid.
package webhook

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

var (
	// ErrUnauthorized is a delivery whose signature did not verify, including every delivery to a resource whose secret is empty.
	ErrUnauthorized = errors.New("webhook: signature did not verify")
	// ErrTooLarge is a body over the resource's max_body.
	ErrTooLarge = errors.New("webhook: body too large")
	// ErrExpression is a filter:/id:/version: that failed to evaluate against this delivery: the pipeline's fault, not the sender's, so it is answered 500 and the delivery can be redelivered once the pipeline is fixed.
	ErrExpression = errors.New("webhook: expression failed")
)

// DefaultMaxBody is the body cap when a resource sets no max_body.
const DefaultMaxBody = 1 << 20

// Request is one delivery as it arrived.
type Request struct {
	Method string
	Header http.Header
	Query  url.Values
	Body   []byte
}

// input is the delivery as an expression sees it.
func (r Request) input(provider, event string) Input {
	return Input{
		Provider: provider,
		Event:    event,
		Method:   r.Method,
		Headers:  flatten(r.Header),
		Query:    flatten(r.Query),
		Body:     string(r.Body),
		Payload:  decodeJSON(r.Body),
	}
}

// Provider is one sender's signature scheme and where it says what happened.
type Provider struct {
	// Verify reports whether the delivery was signed with secret. now is for the schemes that sign a timestamp.
	Verify func(r Request, secret []byte, now time.Time) bool
	// ID is the sender's own delivery id, or "" when it sends none.
	ID func(r Request) string
	// Event is what the sender says happened, or "".
	Event func(r Request) string
	// Handshake, when set, answers a verified delivery that is the sender checking the URL rather than reporting anything.
	Handshake func(r Request) ([]byte, bool)
	// Secret, when set, turns the configured secret into the key; a nil Secret uses its bytes.
	Secret func(raw string) ([]byte, error)
	// Signed names the headers that carry the credential, which never reach headers.json; the first is the one Verify reads.
	Signed []string
}

// Input is what an expression sees: the delivery, already verified.
type Input struct {
	Provider string
	Event    string
	Method   string
	Headers  map[string]string
	Query    map[string]string
	Body     string
	Payload  any
}

// Receiver is one webhook resource, compiled.
type Receiver struct {
	Provider string
	// Scheme is provider: custom's signature, and nil for every other provider.
	Scheme *Scheme
	// SecretEnv names the variable holding the secret; the caller reads it, per delivery, so a rotated secret needs no reload.
	SecretEnv string
	MaxBody   int64
	// Filter, when set, drops a delivery it answers false for.
	Filter func(Input) (bool, error)
	// ID, when set, replaces the sender's delivery id.
	ID func(Input) (string, error)
	// Version projects extra fields into the version, by name.
	Version map[string]func(Input) (string, error)
}

// Delivery is what gets recorded: the version, and the payload a get writes out.
type Delivery struct {
	Version map[string]any
	Body    []byte
	Headers map[string]string
}

// Result is what one delivery came to. At most one of Handshake and Filtered is set; with neither, Delivery is to be recorded.
type Result struct {
	Delivery  Delivery
	Handshake []byte
	Filtered  bool
}

// ReadBody reads a delivery's body under the cap, answering ErrTooLarge rather than a truncated body.
func (r *Receiver) ReadBody(w http.ResponseWriter, req *http.Request) ([]byte, error) {
	limit := r.MaxBody
	if limit <= 0 {
		limit = DefaultMaxBody
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, limit))

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, ErrTooLarge
	}

	if err != nil {
		return nil, fmt.Errorf("webhook: reading the body: %w", err)
	}

	return body, nil
}

// provider is the table entry, or for custom, one built around the resource's own Scheme.
func (r *Receiver) provider() (Provider, bool) {
	if r.Provider == Custom {
		if r.Scheme == nil {
			return Provider{}, false
		}

		return Provider{Verify: r.Scheme.Verify, ID: none, Event: none, Signed: []string{r.Scheme.Header}}, true
	}

	provider, ok := Providers[r.Provider]

	return provider, ok
}

// Verify checks the delivery's signature. An empty secret verifies nothing.
func (r *Receiver) Verify(req Request, secret string, now time.Time) error {
	provider, ok := r.provider()
	if !ok || secret == "" {
		return ErrUnauthorized
	}

	key := []byte(secret)

	if provider.Secret != nil {
		decoded, err := provider.Secret(secret)
		if err != nil {
			return ErrUnauthorized
		}

		key = decoded
	}

	if !provider.Verify(req, key, now) {
		return ErrUnauthorized
	}

	return nil
}

// Accept runs a delivery that has already been verified through the pipeline's own expressions, in order: handshake, filter, id, version.
func (r *Receiver) Accept(request Request) (Result, error) {
	provider, ok := r.provider()
	if !ok {
		return Result{}, fmt.Errorf("webhook: unknown provider %q", r.Provider)
	}

	in := request.input(r.Provider, provider.Event(request))

	if provider.Handshake != nil {
		if reply, ok := provider.Handshake(request); ok {
			return Result{Handshake: reply}, nil
		}
	}

	if r.Filter != nil {
		keep, err := r.Filter(in)
		if err != nil {
			return Result{}, fmt.Errorf("%w: filter: %w", ErrExpression, err)
		}

		if !keep {
			return Result{Filtered: true}, nil
		}
	}

	id, err := r.deliveryID(provider, request, in)
	if err != nil {
		return Result{}, err
	}

	version, err := r.version(in, id)
	if err != nil {
		return Result{}, err
	}

	return Result{Delivery: Delivery{Version: version, Body: request.Body, Headers: kept(request.Header, provider.Signed)}}, nil
}

// version is {id, event, ...what the pipeline projects}; event is left out when the sender names none.
func (r *Receiver) version(in Input, id string) (map[string]any, error) {
	version := map[string]any{"id": id}
	if in.Event != "" {
		version["event"] = in.Event
	}

	for name, project := range r.Version {
		value, err := project(in)
		if err != nil {
			return nil, fmt.Errorf("%w: version.%s: %w", ErrExpression, name, err)
		}

		version[name] = value
	}

	return version, nil
}

// deliveryID is the pipeline's id: when it has one, the sender's own id when it sends one, and random otherwise — never a hash of the body, which would swallow a legitimate repeat.
func (r *Receiver) deliveryID(provider Provider, request Request, in Input) (string, error) {
	if r.ID != nil {
		id, err := r.ID(in)
		if err != nil {
			return "", fmt.Errorf("%w: id: %w", ErrExpression, err)
		}

		if id == "" {
			return "", fmt.Errorf("%w: id: evaluated to an empty string", ErrExpression)
		}

		return id, nil
	}

	if id := provider.ID(request); id != "" {
		return id, nil
	}

	random := make([]byte, 16)
	_, _ = rand.Read(random)

	return hex.EncodeToString(random), nil
}

// flatten keys by lower-case name with the first value: what an expression indexes, and what headers.json holds.
func flatten(values map[string][]string) map[string]string {
	flat := make(map[string]string, len(values))

	for name, vals := range values {
		if len(vals) > 0 {
			flat[strings.ToLower(name)] = vals[0]
		}
	}

	return flat
}

// kept is headers.json: every header but the credentials. The token provider carries the secret itself in Authorization.
func kept(header http.Header, signed []string) map[string]string {
	flat := flatten(header)

	delete(flat, "authorization")
	delete(flat, "cookie")

	for _, name := range signed {
		delete(flat, strings.ToLower(name))
	}

	return flat
}

func decodeJSON(body []byte) any {
	var payload any

	err := json.Unmarshal(body, &payload)
	if err != nil {
		return nil
	}

	return payload
}

// ParseRequest reads a captured delivery: a request line, headers, a blank line, and the body — everything after the blank line, byte for byte, whatever Content-Length says, since a hand-written capture rarely carries one.
func ParseRequest(raw []byte) (Request, error) {
	head, body, found := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !found {
		head, body, found = bytes.Cut(raw, []byte("\n\n"))
	}

	if !found {
		return Request{}, errors.New("webhook: a captured request needs a blank line between its headers and its body")
	}

	// Concat, not append: head shares raw's array with body, and appending would write over the body's first bytes.
	parsed, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(slices.Concat(head, []byte("\r\n\r\n")))))
	if err != nil {
		return Request{}, fmt.Errorf("webhook: reading the captured request: %w", err)
	}

	return Request{Method: parsed.Method, Header: parsed.Header, Query: parsed.URL.Query(), Body: body}, nil
}
