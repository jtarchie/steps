package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// A person watching a run approves the step it waits on from the run page, and lands back on that run rather than on the approvals tab (steps#228). Not t.Parallel(), for startWeb's reason.
func TestARunPageDecidesTheApprovalItWaitsOn(t *testing.T) {
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "1m")

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "publish")

	id := waitForPendingApproval(t, served.state, name)

	approval := listApprovals(t, served.state, name)[0]
	if approval.RunID == "" {
		t.Fatalf("approval %d names no run, so no run page can offer it", id)
	}

	runPage := "/p/" + name + "/runs/" + approval.RunID

	code, body := served.get(t, runPage)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", runPage, code)
	}

	action := fmt.Sprintf(`action="/p/%s/approvals/%d"`, name, id)
	if !strings.Contains(body, action) || !strings.Contains(body, "Draft is ready") {
		t.Fatalf("the run page offers no decision on approval %d (want %s)", id, action)
	}

	code, location := postForm(t, served.addr, fmt.Sprintf("/p/%s/approvals/%d", name, id),
		url.Values{"decision": {"approve"}, "return": {runPage}})
	if code != http.StatusSeeOther || location != runPage {
		t.Fatalf("approving from the run page = %d to %q, want 303 back to %s", code, location, runPage)
	}

	waitForFile(t, filepath.Join(dir, "publish.log"))
}

// postForm submits a form without following the redirect, and returns where it pointed.
func postForm(t *testing.T, addr, path string, form url.Values) (int, string) {
	t.Helper()

	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, resp.Header.Get("Location")
}
