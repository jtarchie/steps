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

	"golang.org/x/crypto/bcrypt"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
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

// Several people, one daemon: each local user is told apart, so each run says its own name. Not t.Parallel(), for startWeb's reason.
func TestEachLocalUserIsToldApart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "daemon.db")
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, `
jobs:
- name: manual
  plan:
  - task: say
    run: echo hi
`)

	usersFile := filepath.Join(dir, "users")
	writePipelineFile(t, usersFile, "# the operators\nsam:"+hashed(t, "sams-password")+"\n")

	served := startWeb(t, "--db", state, "--local-user", "alex:"+hashed(t, "alexs-password"), "--local-users-file", usersFile)
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "alex", "alexs-password")})
	if err != nil {
		t.Fatalf("steps pipeline set as alex: %v", err)
	}

	if code, _ := authGet(t, served.addr, "/p/"+name, "alex", "sams-password"); code != http.StatusUnauthorized {
		t.Errorf("alex with sam's password = %d, want 401", code)
	}

	for _, who := range []struct{ user, pass string }{{"alex", "alexs-password"}, {"sam", "sams-password"}} {
		before := len(listRuns(t, state, name, "manual"))

		if code := authPost(t, served.addr, who.user, who.pass, "/p/"+name+"/jobs/manual/trigger"); code != http.StatusSeeOther {
			t.Fatalf("trigger as %s answered %d, want 303", who.user, code)
		}

		run := waitForNthRun(t, state, name, "manual", before+1)
		waitForAuthedPage(t, served.addr, who.user, who.pass, "/p/"+name+"/runs/"+run.ID, "by "+who.user+" (local)")
	}
}

// A password written in the clear is refused before the daemon listens: it sits in shell history and ps, and the flag exists so it never has to.
func TestALocalUserMustBeAHash(t *testing.T) {
	t.Parallel()

	err := cli.Run([]string{"web", "--listen", "127.0.0.1:1", "--local-user", "alex:hunter2"})
	if err == nil {
		t.Fatal("steps web started with a plain-text password")
	}

	if !strings.Contains(err.Error(), "htpasswd -nbB") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the refusal must name how to hash and must not print the password: %v", err)
	}
}

func hashed(t *testing.T, password string) string {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	return string(hash)
}

// waitForNthRun waits until the job has n runs and returns the newest.
func waitForNthRun(t *testing.T, state, name, job string, n int) store.RunRow {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if runs := listRuns(t, state, name, job); len(runs) >= n {
			return runs[0]
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("job %s never reached %d runs", job, n)

	return store.RunRow{}
}

// A name given twice would let order decide which password works, so it is refused.
func TestALocalUserNamedTwiceIsRefused(t *testing.T) {
	t.Parallel()

	err := cli.Run([]string{"web", "--listen", "127.0.0.1:1",
		"--local-user", "alex:" + hashed(t, "one"), "--local-user", "alex:" + hashed(t, "two")})
	if err == nil || !strings.Contains(err.Error(), `"alex" is given twice`) {
		t.Fatalf("steps web with alex twice = %v, want a refusal naming alex", err)
	}
}
