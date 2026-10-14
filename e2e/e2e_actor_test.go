package e2e

// Who started a run is decided at the request and read on the run page, with the queue and the scheduler's claim between them — so this starts at cli.Run, where a test of the handler or the store alone would pass with the hand-off between them broken.

import (
	"html"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// Not t.Parallel(): startWeb backgrounds cli.Run in this process and stop signals the whole test binary.
func TestARunSaysWhoStartedIt(t *testing.T) {
	const (
		user = "ops"
		pass = "correct-horse-battery"
	)

	dir := t.TempDir()
	state := filepath.Join(dir, "daemon.db")
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, `
resource_types:
- name: counter
  config:
    check: echo '[{"n":"1"}]'
resources:
- name: tick
  type: counter
  source: {}
jobs:
- name: watched
  plan:
  - get: tick
    trigger: true
- name: manual
  plan:
  - task: say
    run: echo hi
`)

	served := startWeb(t, "--db", state, "--interval", "200ms",
		"--basic-auth-username", user, "--basic-auth-password", pass)
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, user, pass)})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	byCheck := newestRun(t, state, name, "watched")
	waitForAuthedPage(t, served.addr, user, pass, "/p/"+name+"/runs/"+byCheck.ID, "by new version of tick")

	if code := authPost(t, served.addr, user, pass, "/p/"+name+"/jobs/manual/trigger"); code != http.StatusSeeOther {
		t.Fatalf("trigger answered %d, want 303", code)
	}

	byPerson := newestRun(t, state, name, "manual")
	waitForAuthedPage(t, served.addr, user, pass, "/p/"+name+"/runs/"+byPerson.ID, "by "+user+" (local)")
}

func authPost(t *testing.T, addr, user, pass, path string) int {
	t.Helper()

	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.SetBasicAuth(user, pass)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode
}

var (
	tags   = regexp.MustCompile(`<[^>]*>`)
	spaces = regexp.MustCompile(`\s+`)
)

// visibleText is a page as a reader sees it: markup gone, whitespace collapsed, entities decoded.
func visibleText(page string) string {
	return html.UnescapeString(spaces.ReplaceAllString(tags.ReplaceAllString(page, ""), " "))
}

// waitForAuthedPage polls until the page says what the caller expects, and fails with the last body it read.
func waitForAuthedPage(t *testing.T, addr, user, pass, path, want string) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(15 * time.Second)
	last := ""

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		req.SetBasicAuth(user, pass)

		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			last = visibleText(string(body))

			if strings.Contains(last, want) {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("GET %s never said %q; last text:\n%s", path, want, last)
}
