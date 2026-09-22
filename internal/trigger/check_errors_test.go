package trigger

// A failing check used to leave nothing behind but a log line on whichever
// machine runs the daemon — and because a poll aborts on the first resource
// that errors, a single broken check silently stops the whole pipeline
// triggering.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// checkErrorFor is the recorded reason for one resource, or "" when nothing is
// recorded.
func checkErrorFor(t *testing.T, st store.Store, resource string) string {
	t.Helper()

	errs, err := st.CheckErrors(t.Context())
	if err != nil {
		t.Fatalf("CheckErrors: %v", err)
	}

	for _, row := range errs {
		if row.Name == resource {
			return row.Message
		}
	}

	return ""
}

// TestAFailingCheckIsRecordedAndClearedWhenItRecovers is the whole seam: the
// poller has the reason and the store has the row, and nothing carried one to
// the other.
func TestAFailingCheckIsRecordedAndClearedWhenItRecovers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")

	cfg := loadConfig(t, dir, strings.Replace(pollFeed, "VERSIONS", versions, 1))
	st := mustOpenStore(t, dir)

	// The file the check cats does not exist yet, so the check exits nonzero.
	_, err := pollOnce(t.Context(), cfg, st)
	if err == nil {
		t.Fatal("a check against a missing file succeeded")
	}

	reason := checkErrorFor(t, st, "items")
	if reason == "" {
		t.Fatal("a failing check recorded no reason")
	}

	if !strings.Contains(reason, "items") {
		t.Errorf("recorded reason %q does not name the resource", reason)
	}

	writeVersions(t, versions, `[{"n":"1"}]`)

	_, err = pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("poll after recovery: %v", err)
	}

	if reason := checkErrorFor(t, st, "items"); reason != "" {
		t.Errorf("a recovered resource is still reported as failing: %q", reason)
	}
}

// TestShuttingDownDoesNotFileAnAlarm: cancelling the daemon cancels every
// check in flight, and filing what those return would leave the next start
// reporting the operator's own Ctrl-C as a broken pipeline.
func TestShuttingDownDoesNotFileAnAlarm(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	writeVersions(t, versions, `[{"n":"1"}]`)

	cfg := loadConfig(t, dir, strings.Replace(pollFeed, "VERSIONS", versions, 1))
	st := mustOpenStore(t, dir)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _ = pollOnce(ctx, cfg, st)

	if reason := checkErrorFor(t, st, "items"); reason != "" {
		t.Errorf("a cancelled poll filed %q", reason)
	}
}
