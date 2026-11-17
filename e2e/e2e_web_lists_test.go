package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// The resources page narrows by status and by what a version holds, and checks a resource on request — one no trigger: true get ever polls included, as Concourse's check button does.
func TestTheResourcesPageNarrowsAndChecksOnRequest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writePipeline(t, dir, `
resource_types:
- name: counter
  config:
    check: echo '[{"n":"1"}]'
- name: commits
  config:
    check: echo '[{"sha":"abc123def"}]'
resources:
- name: tick
  type: counter
  source: {}
- name: quiet
  type: commits
  source: {}
jobs:
- name: watched
  plan:
  - get: tick
    trigger: true
- name: manual
  plan:
  - get: quiet
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)
	page := "/p/" + name + "/resources"

	// The poll at startup checks the trigger resource and never the other.
	waitForServedPage(t, served, page+"?status=ok", `href="/p/`+name+`/resources/tick"`)

	for _, tc := range []struct {
		query        string
		want, refuse []string
	}{
		{"", []string{"/resources/tick", "/resources/quiet", `href="/p/` + name + `/jobs/manual"`}, nil},
		{"?status=unchecked", []string{"/resources/quiet"}, []string{"/resources/tick\""}},
		{"?type=counter", []string{"/resources/tick"}, []string{"/resources/quiet\""}},
		{"?q=abc123", []string{"Nothing matches."}, []string{"/resources/quiet\"", "/resources/tick\""}},
	} {
		code, body := served.get(t, page+tc.query)
		if code != http.StatusOK {
			t.Fatalf("GET %s%s = %d", page, tc.query, code)
		}

		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s%s is missing %s", page, tc.query, want)
			}
		}

		for _, refuse := range tc.refuse {
			if strings.Contains(body, refuse) {
				t.Errorf("%s%s still shows %s", page, tc.query, refuse)
			}
		}
	}

	code, location := postForm(t, served.addr, page+"/quiet/check", url.Values{"return": {page}})
	if code != http.StatusSeeOther || location != page {
		t.Fatalf("checking quiet = %d to %q, want 303 back to %s", code, location, page)
	}

	waitForServedPage(t, served, page+"?q=abc123", `href="/p/`+name+`/resources/quiet"`)
}

// The approvals page is a list like the runs page: what waits is decided on top, what was decided is a history narrowed by status and by what anyone typed, and every row names the run it held.
func TestTheApprovalsPageNarrowsItsHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := approvalPipeline(t, dir, "1m")

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)
	page := "/p/" + name + "/approvals"

	served.trigger(t, name, "publish")
	refused := waitForPendingApproval(t, served.state, name)

	code, _ := postForm(t, served.addr, fmt.Sprintf("%s/%d", page, refused), url.Values{"decision": {"reject"}, "reason": {"Friday freeze"}})
	if code != http.StatusSeeOther {
		t.Fatalf("rejecting = %d, want 303", code)
	}

	served.trigger(t, name, "publish")
	waiting := waitForPendingApproval(t, served.state, name)

	runOf := map[int64]string{}
	for _, approval := range listApprovals(t, served.state, name) {
		runOf[approval.ID] = approval.RunID
	}

	decide := func(id int64) string { return fmt.Sprintf(`action="%s/%d"`, page, id) }
	runLink := func(id int64) string { return fmt.Sprintf(`href="/p/%s/runs/%s"`, name, runOf[id]) }

	// The waiting run is released at the end, or the daemon's shutdown waits on it.
	defer func() {
		_, _ = postForm(t, served.addr, fmt.Sprintf("%s/%d", page, waiting), url.Values{"decision": {"approve"}})
	}()

	for _, tc := range []struct {
		query        string
		want, refuse []string
	}{
		{"", []string{decide(waiting), runLink(waiting), "Friday freeze", runLink(refused)}, nil},
		{"?status=rejected", []string{"Friday freeze", runLink(refused)}, []string{decide(waiting)}},
		{"?status=pending", []string{decide(waiting)}, []string{"Friday freeze"}},
		{"?q=friday", []string{"Friday freeze", `href="` + page + `"`}, []string{decide(waiting)}},
	} {
		code, body := served.get(t, page+tc.query)
		if code != http.StatusOK {
			t.Fatalf("GET %s%s = %d", page, tc.query, code)
		}

		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s%s is missing %s", page, tc.query, want)
			}
		}

		for _, refuse := range tc.refuse {
			if strings.Contains(body, refuse) {
				t.Errorf("%s%s still shows %s", page, tc.query, refuse)
			}
		}
	}
}
