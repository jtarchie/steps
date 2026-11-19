package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// Two operators deciding one approval at once both get the redirect, but only the decision that took effect is in the activity log; a decision on an approval nobody requested is in it not at all.
func TestTheActivityLogRecordsOnlyDecisionsThatTookEffect(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := filepath.Join(dir, "daemon.db")
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, `
jobs:
- name: publish
  plan:
  - approval:
      message: ship it?
      timeout: 1m
`)

	served := startWeb(t, "--db", state, "--local-user", "alex:"+hashed(t, "a"), "--local-user", "sam:"+hashed(t, "s"))
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := stepsRun(t, []string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	if code := authPost(t, served.addr, "alex", "a", "/p/"+name+"/jobs/publish/trigger"); code != http.StatusSeeOther {
		t.Fatalf("trigger answered %d, want 303", code)
	}

	id := waitForPendingApproval(t, state, name)

	for _, decision := range []struct{ user, pass, form string }{
		{"alex", "a", "decision=approve"},
		{"sam", "s", "decision=reject&reason=too+late"},
	} {
		if code := authPostForm(t, served.addr, decision.user, decision.pass, fmt.Sprintf("/p/%s/approvals/%d", name, id), decision.form); code != http.StatusSeeOther {
			t.Fatalf("%s's decision answered %d, want 303", decision.user, code)
		}
	}

	if code := authPostForm(t, served.addr, "sam", "s", fmt.Sprintf("/p/%s/approvals/%d", name, id+1000), "decision=approve"); code != http.StatusSeeOther {
		t.Fatalf("a decision on an approval nobody requested answered %d, want 303", code)
	}

	entries := decisionsLogged(t, state, name)
	if len(entries) != 1 || entries[0].Actor != "local:alex" || entries[0].Action != "approve" || entries[0].Target != fmt.Sprintf("approval %d", id) {
		t.Fatalf("activity = %+v, want only alex's approval, the one decision that took effect", entries)
	}
}

func decisionsLogged(t *testing.T, state, name string) []store.ActivityEntry {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	entries, err := st.Reader().Activity(t.Context(), store.ActivityFilter{Actions: []string{"approve", "reject"}})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}

	return entries
}
