package agent

// The web_fetch builtin: an HTTP GET the pipeline can scope with allow:.
//
// The allowlist is enforced here, in Go, not in prompt language — a model
// told "only fetch from these hosts" can be talked out of it by whatever it
// reads, and this tool exists to read untrusted pages. Every redirect hop is
// re-checked, since a permitted host that 302s elsewhere would otherwise be
// an open door through the list.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

// maxWebFetchBytes bounds the body returned inline — the same designed bound
// as read_file, since both hand the model a document to read. The overflow is
// dropped rather than spilled: unlike a shell tool's output, the page is
// still on the web, and a model that needs the rest can fetch a narrower URL.
const maxWebFetchBytes = 100_000

// webFetchTimeout caps one fetch, independent of the step's own deadline —
// a hung server should cost the conversation one failed tool result, not the
// whole attempt's timeout budget.
const webFetchTimeout = 30 * time.Second

// maxWebFetchRedirects mirrors net/http's own default, made explicit because
// installing a CheckRedirect (for the per-hop allowlist re-check) replaces
// the default that would otherwise enforce it.
const maxWebFetchRedirects = 10

// webFetchTool builds the web_fetch declaration and implementation for one
// grant. Unlike every other builtin, both halves depend on the grant itself:
// allow: narrows what the impl may reach, and the declaration says so, so the
// model does not spend turns trying hosts the tool will refuse.
func webFetchTool(allow []string) (*genai.FunctionDeclaration, toolImpl) {
	decl := &genai.FunctionDeclaration{
		Name:        config.WebFetchBuiltinName,
		Description: webFetchDescription(allow),
		Parameters: objectSchema(map[string]*genai.Schema{
			"url": {Type: genai.TypeString, Description: "The http:// or https:// URL to fetch."},
		}, "url"),
	}

	return decl, execWebFetch(allow)
}

func webFetchDescription(allow []string) string {
	desc := "Fetch a URL over HTTP GET and return its status, content_type, and body text." +
		" Only http:// and https:// URLs. A body longer than the inline cap is cut off (truncated: true in the result)" +
		" — fetch a narrower URL for the rest."
	if len(allow) > 0 {
		desc += fmt.Sprintf(" Restricted to these hosts and their subdomains: %s.", strings.Join(allow, ", "))
	}

	return desc
}

// execWebFetch fetches one URL, reporting every failure — a malformed URL, a
// refused host, a dead server — as tool-result data the model can react to,
// per the toolImpl contract.
func execWebFetch(allow []string) toolImpl {
	return func(ctx context.Context, args map[string]any, _ toolEnv) map[string]any {
		target, errMsg := parseWebFetchURL(args)
		if errMsg != "" {
			return map[string]any{"error": errMsg}
		}

		err := checkWebFetchHost(target, allow)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}

		return doWebFetch(ctx, target, allow)
	}
}

// parseWebFetchURL validates the call's url argument, returning the failure
// as the message to hand the model rather than an error type — every caller
// would only ever wrap it into result data anyway.
func parseWebFetchURL(args map[string]any) (*url.URL, string) {
	raw := stringArg(args, "url")
	if raw == "" {
		return nil, `web_fetch: missing required argument "url"`
	}

	target, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Sprintf("web_fetch: %v", err)
	}

	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Sprintf("web_fetch: unsupported scheme %q — only http and https", target.Scheme)
	}

	return target, ""
}

// doWebFetch performs the GET against an already-validated target, holding
// the allow: fence on every redirect hop.
func doWebFetch(ctx context.Context, target *url.URL, allow []string) map[string]any {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("web_fetch: %v", err)}
	}

	client := &http.Client{
		Timeout: webFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxWebFetchRedirects {
				return fmt.Errorf("stopped after %d redirects", maxWebFetchRedirects)
			}

			return checkWebFetchHost(req.URL, allow)
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("web_fetch: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebFetchBytes+1))
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("web_fetch: reading response body: %v", err)}
	}

	truncated := len(body) > maxWebFetchBytes
	if truncated {
		body = body[:maxWebFetchBytes]
	}

	return map[string]any{
		"status":       resp.StatusCode,
		"content_type": resp.Header.Get("Content-Type"),
		"body":         string(body),
		"truncated":    truncated,
	}
}

// checkWebFetchHost enforces the grant's allow: list against one URL — the
// original target and, via CheckRedirect, every hop after it. An entry
// matches its exact hostname and any subdomain, case-insensitively. An empty
// list allows everything (the bare-grant semantic; see config.ToolSpec.Allow).
func checkWebFetchHost(u *url.URL, allow []string) error {
	if len(allow) == 0 {
		return nil
	}

	host := strings.ToLower(u.Hostname())

	for _, entry := range allow {
		entry = strings.ToLower(entry)
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return nil
		}
	}

	return fmt.Errorf("web_fetch: host %q is not in this tool's allow: list (%s)", host, strings.Join(allow, ", "))
}
