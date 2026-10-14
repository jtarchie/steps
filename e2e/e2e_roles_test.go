package e2e

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// A viewer sees the daemon and cannot change it — not because a button is missing, but because the daemon refuses. Not t.Parallel(), for startWeb's reason.
func TestAViewerCanReadButNotAct(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, "jobs:\n- name: manual\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"),
		"--local-user", "alex:"+hashed(t, "a"), "--local-user", "sam:"+hashed(t, "s"),
		"--role", `admin=UserID == "alex"`)
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "sam", "s")})
	if err == nil {
		t.Fatal("a viewer set a pipeline")
	}

	err = cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("the admin's set: %v", err)
	}

	code, page := authBody(t, served.addr, "sam", "s", "/p/"+name+"/jobs/manual")
	if code != http.StatusOK || strings.Contains(page, "Trigger") || !strings.Contains(page, "you can view·triggering, approving and aborting need the operator role") {
		t.Errorf("a viewer's job page = %d; want no Trigger and the reason why:\n%s", code, page)
	}

	if code := authPost(t, served.addr, "sam", "s", "/p/"+name+"/jobs/manual/trigger"); code != http.StatusForbidden {
		t.Errorf("a viewer's trigger = %d, want refused by the daemon", code)
	}

	if code, _ := authBody(t, served.addr, "sam", "s", "/activity"); code != http.StatusForbidden {
		t.Errorf("a viewer reading the activity log = %d, want refused", code)
	}

	if code := authPost(t, served.addr, "alex", "a", "/p/"+name+"/jobs/manual/trigger"); code != http.StatusSeeOther {
		t.Errorf("the admin's trigger = %d, want 303", code)
	}
}

// Rules are about who is allowed, so a set of them with no admin would leave a daemon nobody could change.
func TestRolesNeedAnAdmin(t *testing.T) {
	t.Parallel()

	err := cli.Run([]string{"web", "--listen", "127.0.0.1:1", "--local-user", "alex:" + hashed(t, "a"), "--role", "operator=true"})
	if err == nil || !strings.Contains(err.Error(), "admin rule") {
		t.Fatalf("steps web with no admin rule = %v, want refused", err)
	}
}
