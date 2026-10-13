package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/telemetry"
	"github.com/jtarchie/steps/internal/web"
)

// daemonClient is the HTTP half of every verb that talks to a daemon: `steps pipeline`, `runs abort` and `mcp login`.
type daemonClient struct {
	// target is the base URL with any userinfo REMOVED, which makes it both the address requests are built against and the one form of it that is safe to print. Every message below names it, and a password that travelled in a flag would otherwise reach a terminal, a CI log and a bug report.
	target string
	// Empty unless the target carried credentials. Held apart so they go in the Authorization header rather than in a URL a redirect or a proxy would carry them into.
	username string
	password string
	http     *http.Client
}

// A set runs every validator the daemon has, so it is not instant — and a daemon that stopped answering must say so rather than hang a terminal.
const daemonTimeout = 30 * time.Second

func newDaemonClient(target string) *daemonClient {
	base, username, password := splitTargetCredentials(target)

	return &daemonClient{
		target:   base,
		username: username,
		password: password,
		// Keep-alives off: a verb makes one or two requests and the process
		// exits, so a pooled connection buys nothing and leaves the daemon
		// holding a socket for a terminal that has gone.
		http: &http.Client{
			Timeout:   daemonTimeout,
			Transport: telemetry.Transport(&http.Transport{DisableKeepAlives: true}),
		},
	}
}

// splitTargetCredentials takes the userinfo off a target URL, returning the address to talk to and the credentials to send. A target that does not parse comes back untouched and credential-free; TargetFlags.Validate refuses one before it gets here.
func splitTargetCredentials(target string) (string, string, string) {
	trimmed := strings.TrimSuffix(target, "/")

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.User == nil {
		return trimmed, "", ""
	}

	username := parsed.User.Username()
	password, _ := parsed.User.Password()
	parsed.User = nil

	return strings.TrimSuffix(parsed.String(), "/"), username, password
}

// held is false for a pipeline the daemon does not serve, which is a set creating one rather than an error; broken is why, when the daemon holds it and a restart could not serve it — a set then repairs it.
func (c *daemonClient) get(name string) (web.PipelineConfig, bool, string, error) {
	var current web.PipelineConfig

	status, body, err := c.do(http.MethodGet, "/api/pipelines/"+name, nil)
	if err != nil {
		return current, false, "", err
	}

	if status == http.StatusNotFound {
		var missing struct {
			Broken string `json:"broken"`
			SHA    string `json:"sha"`
		}

		_ = json.Unmarshal(body, &missing)

		// The held sha, so a repair is compare-and-set against what the daemon holds rather than a blind overwrite of it.
		current.SHA = missing.SHA

		return current, false, missing.Broken, nil
	}

	if status != http.StatusOK {
		return current, false, "", daemonError(c.target, status, body)
	}

	err = json.Unmarshal(body, &current)
	if err != nil {
		return current, false, "", fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return current, true, "", nil
}

func (c *daemonClient) list() ([]web.PipelineSummary, error) {
	status, body, err := c.do(http.MethodGet, "/api/pipelines", nil)
	if err != nil {
		return nil, err
	}

	if status != http.StatusOK {
		return nil, daemonError(c.target, status, body)
	}

	var rows []web.PipelineSummary

	err = json.Unmarshal(body, &rows)
	if err != nil {
		return nil, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return rows, nil
}

func (c *daemonClient) set(name string, req web.SetRequest) (web.SetResult, error) {
	var result web.SetResult

	payload, err := json.Marshal(req)
	if err != nil {
		return result, fmt.Errorf("could not encode the configuration: %w", err)
	}

	status, body, err := c.do(http.MethodPut, "/api/pipelines/"+name, payload)
	if err != nil {
		return result, err
	}

	if status == http.StatusConflict {
		return result, fmt.Errorf("%w — re-read it with `steps pipeline get` and try again", daemonError(c.target, status, body))
	}

	if status != http.StatusOK {
		return result, daemonError(c.target, status, body)
	}

	err = json.Unmarshal(body, &result)
	if err != nil {
		return result, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return result, nil
}

func (c *daemonClient) destroy(name string) error {
	status, body, err := c.do(http.MethodDelete, "/api/pipelines/"+name, nil)
	if err != nil {
		return err
	}

	if status != http.StatusNoContent {
		return daemonError(c.target, status, body)
	}

	return nil
}

func (c *daemonClient) pause(name, verb string) error {
	return c.post("/api/pipelines/"+name+"/"+verb, http.StatusNoContent)
}

// startLogin sends c.target as the address to build the redirect from — the field the credentials were already taken off of.
func (c *daemonClient) startLogin(ctx context.Context, name, server string) (web.LoginStatus, error) {
	payload, err := json.Marshal(web.LoginRequest{Base: c.target})
	if err != nil {
		return web.LoginStatus{}, fmt.Errorf("could not encode the login request: %w", err)
	}

	return c.login(ctx, http.MethodPost, name, server, payload, http.StatusAccepted)
}

func (c *daemonClient) loginStatus(ctx context.Context, name, server string) (web.LoginStatus, error) {
	return c.login(ctx, http.MethodGet, name, server, nil, http.StatusOK)
}

func (c *daemonClient) login(ctx context.Context, method, name, server string, payload []byte, want int) (web.LoginStatus, error) {
	var status web.LoginStatus

	code, body, err := c.doContext(ctx, method, "/api/pipelines/"+name+"/mcp/"+url.PathEscape(server)+"/login", payload)
	if err != nil {
		return status, err
	}

	if code != want {
		return status, daemonError(c.target, code, body)
	}

	err = json.Unmarshal(body, &status)
	if err != nil {
		return status, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return status, nil
}

// post is a bodiless verb whose only answer worth reading is a refusal.
func (c *daemonClient) post(path string, want int) error {
	status, body, err := c.do(http.MethodPost, path, nil)
	if err != nil {
		return err
	}

	if status != want {
		return daemonError(c.target, status, body)
	}

	return nil
}

func (c *daemonClient) rename(from, to string) error {
	payload, err := json.Marshal(map[string]string{"to": to})
	if err != nil {
		return fmt.Errorf("could not encode the new name: %w", err)
	}

	status, body, err := c.do(http.MethodPost, "/api/pipelines/"+from+"/rename", payload)
	if err != nil {
		return err
	}

	if status != http.StatusNoContent {
		return daemonError(c.target, status, body)
	}

	return nil
}

// do is one request, with the body read whatever the status.
func (c *daemonClient) do(method, path string, payload []byte) (int, []byte, error) {
	return c.doContext(context.Background(), method, path, payload)
}

// doContext is do for a verb that waits on something: a login polls for as long as a person takes, and Ctrl-C has to end the request in flight rather than the one after it.
func (c *daemonClient) doContext(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonTimeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.target+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("could not build the request: %w", err)
	}

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Sent and not answered is not unreachable: the change may be mid-way, and "start one" sends somebody to start a second daemon on the same state file.
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			return 0, nil, fmt.Errorf("the steps daemon at %s did not answer within %s, so the change may still be applying — check with: steps pipeline list: %w", c.target, c.http.Timeout, err)
		}

		return 0, nil, fmt.Errorf("could not reach the steps daemon at %s: %w (start one with: steps web)", c.target, err)
	}

	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return resp.StatusCode, answer, nil
}

// The refusal becomes the sentence the terminal that asked prints, which is the whole reason this transport is HTTP rather than a row write.
func daemonError(target string, status int, body []byte) error {
	// The body of a 401 is echo's own "Unauthorized", which names neither what is missing nor where it goes.
	if status == http.StatusUnauthorized {
		return fmt.Errorf("%s asked for credentials it did not get — put them in the target URL: --target http://user:password@host (or STEPS_TARGET)", target)
	}

	message := strings.TrimSpace(string(body))

	// echo wraps a refusal as {"message": ...}, and the message is the part a person acts on.
	var wrapped struct {
		Message string `json:"message"`
	}

	if json.Unmarshal(body, &wrapped) == nil && wrapped.Message != "" {
		message = wrapped.Message
	}

	if message == "" {
		message = http.StatusText(status)
	}

	return fmt.Errorf("%s refused it: %s", target, message)
}
