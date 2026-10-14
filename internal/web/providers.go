package web

// Signing in with an account somebody else keeps — GitHub, Google — through
// goth. The daemon is the OAuth client: it sends the browser to the provider,
// takes the code back, asks who it was, and from then on it is an ordinary
// session (sessions.go) under that person's name.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/markbates/goth"
	"github.com/markbates/goth/providers/github"
	"github.com/markbates/goth/providers/google"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/store"
)

// SignInProvider is one way to sign in that the daemon does not keep the password for.
type SignInProvider struct {
	// Name is the provider's word in URLs and in an actor's kind ("github").
	Name string
	// Label is what the sign-in button says it continues with.
	Label    string
	provider goth.Provider
	client   *http.Client
	// describe turns what the provider said into the person permission rules see.
	describe func(ctx context.Context, client *http.Client, user goth.User) (store.User, error)
}

// signInStartPath sends the browser to a provider; signInCallbackPath is where the provider sends it back, under the daemon's external URL.
const (
	signInStartPath    = "/auth/:provider"
	signInCallbackPath = "/auth/:provider/callback"
)

func callbackURL(base, name string) string {
	return strings.TrimRight(base, "/") + "/auth/" + name + "/callback"
}

// GitHubSignIn signs people in with GitHub, reading their organizations and teams so rules can match "acme/eng". client is nil outside a test that answers for github.com.
func GitHubSignIn(clientID, secret, externalURL string, client *http.Client) SignInProvider {
	provider := github.New(clientID, secret, callbackURL(externalURL, "github"), "read:user", "user:email", "read:org")
	provider.HTTPClient = client

	return SignInProvider{Name: "github", Label: "GitHub", provider: provider, client: client, describe: describeGitHub}
}

// GoogleSignIn signs people in with Google. Google says nothing about groups without its admin API, so rules for these people match on email or domain.
func GoogleSignIn(clientID, secret, externalURL string, client *http.Client) SignInProvider {
	provider := google.New(clientID, secret, callbackURL(externalURL, "google"), "openid", "email", "profile")
	provider.HTTPClient = client

	return SignInProvider{Name: "google", Label: "Google", provider: provider, client: client, describe: describeGoogle}
}

func describeGitHub(ctx context.Context, client *http.Client, user goth.User) (store.User, error) {
	described := store.User{Actor: actor.Actor{Kind: "github", ID: user.NickName}.String(), Email: user.Email}

	var orgs []struct {
		Login string `json:"login"`
	}

	err := githubGet(ctx, client, user.AccessToken, "/user/orgs", &orgs)
	if err != nil {
		return store.User{}, err
	}

	for _, org := range orgs {
		described.Organizations = append(described.Organizations, org.Login)
	}

	var teams []struct {
		Slug         string `json:"slug"`
		Organization struct {
			Login string `json:"login"`
		} `json:"organization"`
	}

	err = githubGet(ctx, client, user.AccessToken, "/user/teams", &teams)
	if err != nil {
		return store.User{}, err
	}

	for _, team := range teams {
		described.Groups = append(described.Groups, team.Organization.Login+"/"+team.Slug)
	}

	return described, nil
}

// githubAPI is GitHub's API root; goth's own requests use the same host.
const githubAPI = "https://api.github.com"

func githubGet(ctx context.Context, client *http.Client, token, path string, into any) error {
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+path+"?per_page=100", nil)
	if err != nil {
		return fmt.Errorf("github %s: %w", path, err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github %s: %w", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github %s: %s", path, resp.Status)
	}

	err = json.NewDecoder(resp.Body).Decode(into)
	if err != nil {
		return fmt.Errorf("github %s: %w", path, err)
	}

	return nil
}

func describeGoogle(_ context.Context, _ *http.Client, user goth.User) (store.User, error) {
	if user.Email == "" {
		return store.User{}, errors.New("google gave no email address")
	}

	described := store.User{Actor: actor.Actor{Kind: "google", ID: user.Email}.String(), Email: user.Email}

	// hd is the Workspace domain, absent for a personal account.
	if domain, ok := user.RawData["hd"].(string); ok && domain != "" {
		described.Organizations = []string{domain}
	}

	return described, nil
}

// WithSignInProviders adds the providers the sign-in page offers.
func WithSignInProviders(providers ...SignInProvider) Option {
	return func(s *Server) {
		s.providers = append(s.providers, providers...)
		s.authFor()
	}
}

func (s *Server) providerNamed(name string) (SignInProvider, bool) {
	for _, provider := range s.providers {
		if provider.Name == name {
			return provider, true
		}
	}

	return SignInProvider{}, false
}

// pendingSignIn is a trip to a provider not yet back: the goth session that started it and where to land after. Held in memory, keyed by the state the provider hands back, because one daemon is the only thing that will ever be asked for it.
type pendingSignIn struct {
	provider string
	session  string
	next     string
	expires  time.Time
}

// signInWait is how long a trip to a provider may take before its state is forgotten.
const signInWait = 10 * time.Minute

type pendingSignIns struct {
	mu      sync.Mutex
	entries map[string]pendingSignIn
}

func (p *pendingSignIns) put(state string, entry pendingSignIn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.entries == nil {
		p.entries = map[string]pendingSignIn{}
	}

	now := time.Now()
	for key, old := range p.entries {
		if now.After(old.expires) {
			delete(p.entries, key)
		}
	}

	p.entries[state] = entry
}

// take is single-use: a state answered once cannot be answered again.
func (p *pendingSignIns) take(state string) (pendingSignIn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, found := p.entries[state]
	delete(p.entries, state)

	return entry, found && time.Now().Before(entry.expires)
}

// handleSignInStart sends the browser to the provider.
func (s *Server) handleSignInStart(c *echo.Context) error {
	provider, found := s.providerNamed(c.Param("provider"))
	if !found {
		return echo.NewHTTPError(http.StatusNotFound, "no such way to sign in")
	}

	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	state := base64.RawURLEncoding.EncodeToString(raw)

	session, err := provider.provider.BeginAuth(state)
	if err != nil {
		return echoError(err)
	}

	to, err := session.GetAuthURL()
	if err != nil {
		return echoError(err)
	}

	s.pending.put(state, pendingSignIn{provider: provider.Name, session: session.Marshal(), next: safeNext(c.QueryParam("next")), expires: time.Now().Add(signInWait)})

	//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
	return c.Redirect(http.StatusSeeOther, to)
}

// handleSignInCallback is the browser back from the provider: a state this daemon handed out, and a code to exchange. Anything else is a sign-in the browser did not start here, refused without detail.
func (s *Server) handleSignInCallback(c *echo.Context) error {
	ctx := c.Request().Context()

	pending, found := s.pending.take(c.QueryParam("state"))
	if !found || pending.provider != c.Param("provider") {
		return s.renderLogin(c, http.StatusBadRequest, "", "that sign-in expired or was not started here — try again")
	}

	provider, _ := s.providerNamed(pending.provider)

	who, err := s.completeSignIn(ctx, provider, pending.session, c.Request().URL.Query())
	if err != nil {
		slog.WarnContext(ctx, "web.signin_failed", "provider", provider.Name, "error", err)

		return s.renderLogin(c, http.StatusUnauthorized, "", "couldn't sign you in with "+provider.Label)
	}

	err = s.startSession(c, who)
	if err != nil {
		return echoError(err)
	}

	//nolint:wrapcheck // as above
	return c.Redirect(http.StatusSeeOther, pending.next)
}

func (s *Server) completeSignIn(ctx context.Context, provider SignInProvider, saved string, params url.Values) (actor.Actor, error) {
	session, err := provider.provider.UnmarshalSession(saved)
	if err != nil {
		return actor.Actor{}, fmt.Errorf("restore session: %w", err)
	}

	_, err = session.Authorize(provider.provider, params)
	if err != nil {
		return actor.Actor{}, fmt.Errorf("exchange code: %w", err)
	}

	user, err := provider.provider.FetchUser(session)
	if err != nil {
		return actor.Actor{}, fmt.Errorf("fetch user: %w", err)
	}

	described, err := provider.describe(ctx, provider.client, user)
	if err != nil {
		return actor.Actor{}, err
	}

	described.RefreshedAt = time.Now()

	err = s.sessions.SaveUser(ctx, described)
	if err != nil {
		return actor.Actor{}, err //nolint:wrapcheck // the store names the user
	}

	return actor.Parse(described.Actor), nil
}
