package e2e

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// A pipeline's access: narrows what the daemon allows and never widens it: the file is something a pull request can change, so it must not be able to grant itself more. Not t.Parallel(), for startWeb's reason.
func TestAPipelineCanOnlyNarrowAccess(t *testing.T) {
	dir := t.TempDir()

	public := filepath.Join(dir, "public.yml")
	writePipelineFile(t, public, "jobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	secret := filepath.Join(dir, "secret.yml")
	writePipelineFile(t, secret, `
access:
  viewer: UserID != "eve"
  operator: "true"
jobs:
- name: deploy
  plan:
  - task: say
    run: echo hi
`)

	args := make([]string, 0, 14)
	args = append(args, "--db", filepath.Join(dir, "daemon.db"), "--role", `admin=UserID == "alex"`, "--role", `operator=UserID == "dan"`)

	for _, user := range []string{"alex", "dan", "sam", "eve"} {
		args = append(args, "--local-user", user+":"+hashed(t, user))
	}

	served := startWeb(t, args...)
	defer served.stopIfRunning(t)

	for name, path := range map[string]string{"public": public, "secret": secret} {
		err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "alex", "alex")})
		if err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	if code := authPost(t, served.addr, "sam", "sam", "/p/secret/jobs/deploy/trigger"); code != http.StatusForbidden {
		t.Errorf("sam, a viewer on the daemon, triggered in a pipeline whose operator rule is true (%d): the file widened access", code)
	}

	if code := authPost(t, served.addr, "dan", "dan", "/p/secret/jobs/deploy/trigger"); code != http.StatusSeeOther {
		t.Errorf("dan, an operator both places, could not trigger (%d)", code)
	}

	if code, _ := authBody(t, served.addr, "eve", "eve", "/p/secret"); code != http.StatusNotFound {
		t.Errorf("eve, whom the pipeline hides, got %d for it, want told it does not exist", code)
	}

	if _, root := authBody(t, served.addr, "eve", "eve", "/"); strings.Contains(root, "secret") || !strings.Contains(root, "public") {
		t.Errorf("eve's root lists the hidden pipeline, or not the public one:\n%s", root)
	}

	if _, root := authBody(t, served.addr, "sam", "sam", "/"); !strings.Contains(root, "secret") {
		t.Error("sam may see the secret pipeline but it is not listed")
	}
}

// A rule that cannot compile is refused at set, on the machine that would serve it, rather than quietly hiding the pipeline. Not t.Parallel(), for startWeb's reason.
func TestAnAccessRuleThatCannotWorkIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.yml")
	writePipelineFile(t, path, "access:\n  viewer: 'UserID =='\njobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--local-user", "alex:"+hashed(t, "a"))
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", "broken", "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err == nil || !strings.Contains(err.Error(), "access.viewer") {
		t.Fatalf("set with a broken access rule = %v, want refused naming access.viewer", err)
	}
}
