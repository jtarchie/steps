package resource

// The HTTP half of the github-* types: authentication, retries, conditional
// requests, and GitHub's error shape. Nothing here knows what a pull request
// is; github.go does.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

const (
	githubJSON    = "application/vnd.github+json"
	githubDiff    = "application/vnd.github.diff"
	githubVersion = "2022-11-28"
	// githubMaxBody bounds a JSON or diff answer read into memory. A tarball
	// is streamed instead and bounded by what it unpacks to.
	githubMaxBody = 64 << 20
	// githubCallTimeout is one API request; a tarball gets githubFetchTimeout.
	githubCallTimeout  = 2 * time.Minute
	githubFetchTimeout = 30 * time.Minute
	// githubRetries is how many times a throttled or briefly-unavailable
	// request is sent again, and githubMaxWait the longest Retry-After
	// honored: a longer one is a rate limit, and waiting it out inside a
	// poll would hold the poll hostage.
	githubRetries = 3
	githubMaxWait = 60 * time.Second
)

var errGitHubToken = errors.New("no GitHub token")

type githubClient struct {
	api     string
	graphql string
	token   string
	// cacheKey keeps two tokens' conditional requests apart: an ETag is an
	// answer to one identity, and a private repository answers two
	// differently.
	cacheKey string
}

func newGitHubClient(connection config.GitHubConnection) (*githubClient, error) {
	token := os.Getenv(connection.Token())
	if token == "" {
		return nil, fmt.Errorf("%w: $%s is not set (source.token_env)", errGitHubToken, connection.Token())
	}

	api := connection.API()
	graphql := api + "/graphql"

	// A GitHub Enterprise Server serves REST under /api/v3 and GraphQL beside it.
	if host, ok := strings.CutSuffix(api, "/api/v3"); ok {
		graphql = host + "/api/graphql"
	}

	sum := sha256.Sum256([]byte(token))

	return &githubClient{api: api, graphql: graphql, token: token, cacheKey: hex.EncodeToString(sum[:8])}, nil
}

type githubReply struct {
	status int
	body   []byte
}

// get is a REST GET relative to the API base, answered from the conditional
// cache when GitHub says nothing changed — which costs no rate limit.
func (c *githubClient) get(ctx context.Context, path, accept string) (githubReply, error) {
	return c.send(ctx, http.MethodGet, c.api+path, accept, nil)
}

func (c *githubClient) send(ctx context.Context, method, target, accept string, payload any) (githubReply, error) {
	encoded, err := encodePayload(payload)
	if err != nil {
		return githubReply{}, fmt.Errorf("%s %s: %w", method, target, err)
	}

	accept = cmpOr(accept, githubJSON)
	cacheKey := c.cacheKey + " " + accept + " " + target
	cached, hasCached := githubETags.lookup(cacheKey)

	for attempt := 0; ; attempt++ {
		reply, etag, wait, err := c.once(ctx, method, target, accept, encoded, cached.etag)
		if err != nil {
			return githubReply{}, err
		}

		if reply.status == http.StatusNotModified && hasCached {
			return githubReply{status: http.StatusOK, body: cached.body}, nil
		}

		if !worthRetrying(method, reply.status, wait, attempt) {
			rememberETag(cacheKey, method, reply, etag)

			return reply, nil
		}

		err = sleepCtx(ctx, wait)
		if err != nil {
			return githubReply{}, fmt.Errorf("%s %s: %w", method, target, err)
		}
	}
}

func worthRetrying(method string, status int, wait time.Duration, attempt int) bool {
	return wait >= 0 && attempt < githubRetries && retryable(method, status)
}

func rememberETag(cacheKey, method string, reply githubReply, etag string) {
	if method == http.MethodGet && reply.status == http.StatusOK && etag != "" {
		githubETags.store(cacheKey, etagEntry{etag: etag, body: reply.body})
	}
}

func encodePayload(payload any) ([]byte, error) {
	if payload == nil {
		return nil, nil
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return encoded, nil
}

// once sends one request. retryAfter is how long to wait before sending it
// again, or negative when GitHub asked for longer than is worth waiting.
func (c *githubClient) once(ctx context.Context, method, target, accept string, payload []byte, etag string) (githubReply, string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, githubCallTimeout)
	defer cancel()

	request, err := c.request(ctx, method, target, accept, payload)
	if err != nil {
		return githubReply{}, "", 0, err
	}

	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return githubReply{}, "", 0, fmt.Errorf("%s %s: %w", method, target, err)
	}

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, githubMaxBody+1))
	if err != nil {
		return githubReply{}, "", 0, fmt.Errorf("%s %s: reading the answer: %w", method, target, err)
	}

	if len(body) > githubMaxBody {
		return githubReply{}, "", 0, fmt.Errorf("%s %s: the answer is larger than %d bytes", method, target, githubMaxBody)
	}

	return githubReply{status: response.StatusCode, body: body}, response.Header.Get("ETag"), retryDelay(response.Header, attemptBackoff), nil
}

func (c *githubClient) request(ctx context.Context, method, target, accept string, payload []byte) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, target, err)
	}

	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", githubVersion)
	request.Header.Set("User-Agent", "steps")

	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	return request, nil
}

// download streams a GET, for a tarball too large to hold. GitHub answers
// the tarball URL with a redirect to codeload; the client follows it, and
// drops the Authorization header on the way because the host changes — the
// redirect URL carries its own short-lived grant.
func (c *githubClient) download(ctx context.Context, path string) (io.ReadCloser, context.CancelFunc, error) {
	target := c.api + path

	for attempt := 0; ; attempt++ {
		fetchCtx, cancel := context.WithTimeout(ctx, githubFetchTimeout)

		request, err := c.request(fetchCtx, http.MethodGet, target, githubJSON, nil)
		if err != nil {
			cancel()

			return nil, nil, err
		}

		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()

			return nil, nil, fmt.Errorf("GET %s: %w", target, err)
		}

		if response.StatusCode == http.StatusOK {
			return response.Body, cancel, nil
		}

		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		_ = response.Body.Close()

		cancel()

		wait := retryDelay(response.Header, attemptBackoff)
		if !worthRetrying(http.MethodGet, response.StatusCode, wait, attempt) {
			return nil, nil, githubFailure(http.MethodGet, path, githubReply{status: response.StatusCode, body: body})
		}

		err = sleepCtx(ctx, wait)
		if err != nil {
			return nil, nil, fmt.Errorf("GET %s: %w", target, err)
		}
	}
}

// attemptBackoff is the wait when GitHub names none. A variable so a test
// can take it to zero.
var attemptBackoff = time.Second //nolint:gochecknoglobals // a test seam, read-only in production

// retryable is the statuses worth sending again. A POST only on 429,
// because a 502 after a comment was accepted would post it twice.
func retryable(method string, status int) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return method != http.MethodPost
	default:
		return false
	}
}

// retryDelay reads Retry-After, and a secondary rate limit's reset. Negative
// means longer than is worth waiting inside one stage.
func retryDelay(header http.Header, fallback time.Duration) time.Duration {
	raw := header.Get("Retry-After")
	if raw == "" {
		return fallback
	}

	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}

	wait := time.Duration(seconds) * time.Second
	if wait > githubMaxWait {
		return -1
	}

	return wait
}

func sleepCtx(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("%w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// githubFailure turns an answer that was not the one asked for into an
// error carrying GitHub's own words, which name the actual problem far more
// often than the status does.
func githubFailure(method, path string, reply githubReply) error {
	var shaped struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"errors"`
	}

	message := strings.TrimSpace(string(reply.body))

	if json.Unmarshal(reply.body, &shaped) == nil && shaped.Message != "" {
		var built strings.Builder

		built.WriteString(shaped.Message)

		for _, detail := range shaped.Errors {
			if said := cmpOr(detail.Message, detail.Code); said != "" {
				built.WriteString("; " + said)
			}
		}

		message = built.String()
	}

	if len(message) > 500 {
		message = message[:500] + "…"
	}

	return fmt.Errorf("github: %s %s: %d %s: %s", method, path, reply.status, http.StatusText(reply.status), message)
}

// getJSON is a GET that must answer 200 with a JSON document.
func (c *githubClient) getJSON(ctx context.Context, path string, out any) ([]byte, error) {
	reply, err := c.get(ctx, path, githubJSON)
	if err != nil {
		return nil, err
	}

	if reply.status != http.StatusOK {
		return nil, githubFailure(http.MethodGet, path, reply)
	}

	if out != nil {
		err = json.Unmarshal(reply.body, out)
		if err != nil {
			return nil, fmt.Errorf("github: GET %s: %w", path, err)
		}
	}

	return reply.body, nil
}

// login is the token's own user, which is what "@me" means.
func (c *githubClient) login(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}

	_, err := c.getJSON(ctx, "/user", &user)
	if err != nil {
		return "", err
	}

	return user.Login, nil
}

type etagEntry struct {
	etag string
	body []byte
}

// etagCache holds the last answer to each conditional GET. It is a cost
// saving and never a semantic one: a miss re-asks and gets the same answer,
// so it may be dropped whole whenever it is full.
type etagCache struct {
	mu      sync.Mutex
	entries map[string]etagEntry
}

const etagCacheLimit = 512

var githubETags = &etagCache{entries: map[string]etagEntry{}} //nolint:gochecknoglobals // one cache per process, as HTTP caches are

func (e *etagCache) lookup(key string) (etagEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	entry, ok := e.entries[key]

	return entry, ok
}

func (e *etagCache) store(key string, entry etagEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.entries) >= etagCacheLimit {
		e.entries = map[string]etagEntry{}
	}

	e.entries[key] = entry
}
