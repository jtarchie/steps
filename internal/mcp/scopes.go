package mcp

// Whether a saved login is ALLOWED to do the work, which is a different
// question from whether it is valid. tools/list answers for any token the
// server accepts, so a login can pass every check in inspect.go and every
// probe, and still be refused on each call a model makes: the server renamed
// its permissions (Metabase did, and every saved login kept the old names),
// or auth.scopes: changed and nobody logged in again.
//
// Three sources, each asked as cheaply as it can be:
//   - the saved login alone: what it asked for and what it was granted,
//     against what the pipeline asks for now. No request.
//   - the server's protected-resource metadata (RFC 9728 scopes_supported):
//     one unauthenticated GET, made only by a probe, which preflight caches.
//   - a refusal the server actually gave: a tool call answered 403
//     insufficient_scope. Remembered against the login that earned it, so the
//     next preflight says so instead of the next model finding out.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/jtarchie/steps/internal/config"
)

// offlineAccess is an authorization server's scope, not the resource's: a
// login asks for it to get a refresh token (SEP-2207), and a resource's
// scopes_supported never lists it. Every comparison here leaves it out, or a
// correct login would read as one asking for a scope the server lacks.
const offlineAccess = "offline_access"

// grantedScopes is the scope a token response named, or nil when it named none.
func grantedScopes(tok *oauth2.Token) []string {
	granted, _ := tok.Extra("scope").(string)

	return resourceScopes(strings.Fields(granted))
}

// resourceScopes is scopes without offline_access, sorted, nil when empty.
func resourceScopes(scopes []string) []string {
	var out []string

	for _, scope := range scopes {
		if scope != offlineAccess && !slices.Contains(out, scope) {
			out = append(out, scope)
		}
	}

	slices.Sort(out)

	return out
}

// effectiveGrant is what a login can be taken to hold: what the server said
// it granted, or else what it asked for, which RFC 6749 §5.1 says an omitted
// scope means. Nil when neither is known — a login that asked for everything
// from a server that never said what everything was.
func (t *TokenFile) effectiveGrant() []string {
	if t.Granted != nil {
		return t.Granted
	}

	return resourceScopes(t.Scopes)
}

// ScopeRefusal is a server refusing a tool call for want of a permission the
// credential lacks. It is not something a model can act on — no argument it
// could change makes the call succeed — so a step that meets one ends as
// errored, and an oauth one is remembered against its login.
type ScopeRefusal struct {
	Pipeline string
	Server   string
	Tool     string
	// Required is the scope the server named, when it named one.
	Required string
	// Detail is the server's own words, bounded, for a refusal that named no scope.
	Detail string
	oauth  bool
	envVar string
}

func (r *ScopeRefusal) Error() string {
	needs := "a permission this credential lacks"
	if r.Required != "" {
		needs = fmt.Sprintf("scope %q, which this credential lacks", r.Required)
	} else if r.Detail != "" {
		needs += " (" + r.Detail + ")"
	}

	msg := fmt.Sprintf("mcp server %q refused %s: it needs %s", r.Server, r.Tool, needs)

	switch {
	case r.oauth:
		return msg + fmt.Sprintf(" (%v %s with -p %s again)", ErrNeedsLogin, r.Server, r.Pipeline)
	case r.envVar != "":
		return msg + fmt.Sprintf(" (the token in $%s is the one to widen)", r.envVar)
	default:
		return msg
	}
}

// Unwrap makes an oauth refusal ErrNeedsLogin, which is what it is: a login
// fixes it and nothing else does, so a probe re-derives it rather than
// remembering it past the login that clears it.
func (r *ScopeRefusal) Unwrap() error {
	if r.oauth {
		return ErrNeedsLogin
	}

	return nil
}

// refusalKey names a login the way a run reaches it, endpoint included: two
// pipelines may each call a server "tracker", and a login is per pipeline.
type refusalKey struct {
	pipeline, server, endpoint string
}

type refusalRecord struct {
	refusal *ScopeRefusal
	at      time.Time
	// loggedIn is the refused login's LoggedIn. A different one is a newer
	// login, and the refusal does not speak for it.
	loggedIn time.Time
}

// refusals is held in memory, not in the state database. Everything that
// reads it — preflight and the mcp tab — runs in the process that ran the
// refused call, which under `steps web` is the daemon that holds the login. A
// restart forgets it, and costs one more refused call to learn it again; the
// checks below that need no refusal survive restarts on their own.
var refusals sync.Map

func noteRefusal(ctx context.Context, pipeline string, srv config.MCPServer, refusal *ScopeRefusal) {
	if !srv.Auth.OAuth() {
		return
	}

	tf, err := loadLogin(ctx, LoginKey{pipeline, srv.Name})
	if err != nil || tf == nil {
		return
	}

	refusals.Store(refusalKey{pipeline, srv.Name, srv.Endpoint}, refusalRecord{refusal: refusal, at: time.Now(), loggedIn: tf.LoggedIn})
}

// standingRefusal is the refusal tf's own login was given, if one was.
func standingRefusal(pipeline string, srv config.MCPServer, tf *TokenFile) *refusalRecord {
	key := refusalKey{pipeline, srv.Name, srv.Endpoint}

	held, ok := refusals.Load(key)
	if !ok {
		return nil
	}

	record, _ := held.(refusalRecord)
	if !record.loggedIn.Equal(tf.LoggedIn) {
		refusals.CompareAndDelete(key, held)

		return nil
	}

	return &record
}

// staleLogin is why tf cannot do what srv's pipeline asks of it, judged from
// the saved login alone, as an error that wraps ErrNeedsLogin and names the
// login to make: every answer is fixed by one. Nil when none applies.
func staleLogin(pipeline string, srv config.MCPServer, tf *TokenFile) error {
	reason := staleReason(pipeline, srv, tf)
	if reason == "" {
		return nil
	}

	return fmt.Errorf("mcp server %q: %s %w", srv.Name, reason, loginAgain{fmt.Sprintf("(%v %s with -p %s again)", ErrNeedsLogin, srv.Name, pipeline)})
}

// staleReason is staleLogin's sentence without the server or the command,
// which is what a page with a Connect button beside it wants: a refusal the
// login was already given, auth.scopes: changed since it was made, or a scope
// it asked for and was not granted. "" when none applies.
func staleReason(pipeline string, srv config.MCPServer, tf *TokenFile) string {
	if record := standingRefusal(pipeline, srv, tf); record != nil {
		return fmt.Sprintf("refused %s at %s for want of %s, and nobody has logged in since",
			record.refusal.Tool, record.at.Format(time.Kitchen), wanted(record.refusal))
	}

	configured := resourceScopes(srv.Auth.Scopes)
	asked := resourceScopes(tf.Scopes)

	if !slices.Equal(configured, asked) {
		was := "everything the server offered"
		if len(asked) > 0 {
			was = quoteScopes(asked)
		}

		now := "everything the server offers"
		if len(configured) > 0 {
			now = quoteScopes(configured)
		}

		return fmt.Sprintf("auth.scopes asks for %s, but this login asked for %s", now, was)
	}

	if tf.Granted != nil {
		if missing := without(configured, tf.Granted); len(missing) > 0 {
			return fmt.Sprintf("auth.scopes asks for %s, which this login was not granted (it holds %s)",
				quoteScopes(missing), quoteScopes(tf.Granted))
		}
	}

	return ""
}

// loginAgain carries the "run steps mcp login … again" clause and makes the
// error it ends ErrNeedsLogin.
type loginAgain struct{ text string }

func (l loginAgain) Error() string { return l.text }
func (loginAgain) Unwrap() error   { return ErrNeedsLogin }

func wanted(refusal *ScopeRefusal) string {
	if refusal.Required != "" {
		return fmt.Sprintf("scope %q", refusal.Required)
	}

	return "a permission it lacks"
}

// CheckScopes reports why srv's saved login cannot do what the pipeline asks
// of it, adding to staleLogin what only the server can say: which scopes it
// offers now. Only an oauth server has a login to judge, and a missing or
// unusable login is checkCredential's to report, so both answer nil here.
//
// The metadata is fetched on every call, so this belongs behind a cache:
// preflight's, which is where it is called from.
func CheckScopes(ctx context.Context, pipeline string, srv config.MCPServer) error {
	if !srv.Auth.OAuth() {
		return nil
	}

	tf, err := loadLogin(ctx, LoginKey{pipeline, srv.Name})
	if err != nil || tf == nil || tf.Endpoint != srv.Endpoint {
		return nil //nolint:nilerr // checkCredential reports an unusable login, in the words a run already uses
	}

	err = staleLogin(pipeline, srv, tf)
	if err != nil {
		return err
	}

	return checkOffered(ctx, pipeline, srv, tf)
}

// StaleLogin is CheckScopes without the request: what the saved login alone
// says. Cheap enough to run where a cached verdict would otherwise answer,
// which is the point — a refusal arrives mid-run, and preflight's cache would
// otherwise vouch for the login for minutes after it.
func StaleLogin(ctx context.Context, pipeline string, srv config.MCPServer) error {
	if !srv.Auth.OAuth() {
		return nil
	}

	tf, err := loadLogin(ctx, LoginKey{pipeline, srv.Name})
	if err != nil || tf == nil || tf.Endpoint != srv.Endpoint {
		return nil //nolint:nilerr // checkCredential reports an unusable login, in the words a run already uses
	}

	return staleLogin(pipeline, srv, tf)
}

// checkOffered judges tf against the scopes the server advertises now.
func checkOffered(ctx context.Context, pipeline string, srv config.MCPServer, tf *TokenFile) error {
	prm, err := discoverProtectedResourceMetadata(ctx, srv.Endpoint)
	if err != nil || len(prm.ScopesSupported) == 0 {
		// The metadata is optional to a server and says nothing about this
		// login when absent. tools/list just answered, so this is not the
		// server being down either.
		slog.DebugContext(ctx, "mcp.scopes.unadvertised", "server", srv.Name, "error", err)

		return nil
	}

	offered := resourceScopes(prm.ScopesSupported)

	// Not a stale login: no login can be granted a scope the server does not
	// have. The pipeline names it, so the pipeline is what changes.
	if missing := without(resourceScopes(srv.Auth.Scopes), offered); len(missing) > 0 {
		return fmt.Errorf("mcp server %q: auth.scopes names %s, which the server does not offer (it offers %s)",
			srv.Name, quoteScopes(missing), quoteScopes(offered))
	}

	// Sharing none at all is a login from before the server changed its
	// names, and nothing else: a login holding SOME of what is offered is
	// the normal case, since a grant is supposed to be narrower than
	// everything.
	if held := tf.effectiveGrant(); len(held) > 0 && len(without(held, offered)) == len(held) {
		return fmt.Errorf("mcp server %q: this login holds %s, none of which the server offers any more (it offers %s), so it predates a change on the server %w",
			srv.Name, quoteScopes(held), quoteScopes(offered), loginAgain{fmt.Sprintf("(%v %s with -p %s again)", ErrNeedsLogin, srv.Name, pipeline)})
	}

	return nil
}

// without is want minus have.
func without(want, have []string) []string {
	var missing []string

	for _, scope := range want {
		if !slices.Contains(have, scope) {
			missing = append(missing, scope)
		}
	}

	return missing
}

func quoteScopes(scopes []string) string {
	quoted := make([]string, len(scopes))
	for i, scope := range scopes {
		quoted[i] = fmt.Sprintf("%q", scope)
	}

	return strings.Join(quoted, ", ")
}

// refusalSlot is where scopeWatch leaves what a 403 said, for the CallTool
// whose request drew it. It travels in the request's context, which is the
// one thing that ties an HTTP response back to the call that made it when
// several calls share a session.
type refusalSlot struct {
	mu   sync.Mutex
	seen *scopeChallenge
}

type refusalSlotKey struct{}

type scopeChallenge struct {
	required string
	detail   string
}

func withRefusalSlot(ctx context.Context) (context.Context, *refusalSlot) {
	slot := &refusalSlot{}

	return context.WithValue(ctx, refusalSlotKey{}, slot), slot
}

func (s *refusalSlot) set(challenge *scopeChallenge) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seen = challenge
}

func (s *refusalSlot) get() *scopeChallenge {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.seen
}

// scopeWatch reads a 403 for the one thing the SDK's error drops: whether it
// was a refusal for want of a scope. The SDK hands the caller "rejected by
// transport: Forbidden" and the body as text, which no caller should parse.
type scopeWatch struct {
	base http.RoundTripper
}

func (w scopeWatch) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := w.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		return resp, err //nolint:wrapcheck // a RoundTripper middleware must pass its base's error through untouched
	}

	slot, _ := req.Context().Value(refusalSlotKey{}).(*refusalSlot)
	if slot == nil {
		return resp, nil
	}

	if challenge := readScopeChallenge(resp); challenge != nil {
		slot.set(challenge)
	}

	return resp, nil
}

var (
	challengeParam = regexp.MustCompile(`(scope|error_description)="([^"]*)"`)
	// Metabase names the scope in its body and sends no challenge:
	// `Insufficient scope to call tool: "get_content". Requires "agent:content:read"; …`.
	bodyRequires = regexp.MustCompile(`Requires "([^"]+)"`)
)

// maxRefusalBody bounds how much of a 403's body is read to judge it. A
// refusal is a sentence; the rest is put back for the SDK untouched.
const maxRefusalBody = 4 << 10

// readScopeChallenge is the RFC 6750 insufficient_scope challenge on resp, or
// failing that a body that says "insufficient scope" in words, or nil for a
// 403 that is about something else (a resource this account may not see).
func readScopeChallenge(resp *http.Response) *scopeChallenge {
	for _, value := range resp.Header.Values("WWW-Authenticate") {
		if !strings.Contains(value, "insufficient_scope") {
			continue
		}

		challenge := &scopeChallenge{}

		for _, match := range challengeParam.FindAllStringSubmatch(value, -1) {
			if match[1] == "scope" {
				challenge.required = match[2]
			} else {
				challenge.detail = bounded(match[2])
			}
		}

		return challenge
	}

	head, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}

	text := strings.ToLower(string(head))
	if !strings.Contains(text, "insufficient scope") && !strings.Contains(text, "insufficient_scope") {
		return nil
	}

	challenge := &scopeChallenge{detail: bounded(strings.TrimSpace(string(head)))}
	if match := bodyRequires.FindStringSubmatch(string(head)); match != nil {
		challenge.required = match[1]
	}

	return challenge
}

func bounded(text string) string {
	const limit = 300
	if len(text) <= limit {
		return text
	}

	return text[:limit] + "…"
}
