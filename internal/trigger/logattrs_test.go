package trigger

// A daemon polls several pipelines at once, so a line that says a check failed
// is only useful if it says whose.

import (
	"bytes"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

// captureLog installs a default logger wrapped the way the CLI wraps its own,
// for the rest of the test. Callers are not t.Parallel().
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(events.LogHandler(slog.NewTextHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return &buf
}

// logLine is the first captured line carrying msg, or "".
func logLine(out, msg string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "msg="+msg+" ") {
			return line
		}
	}

	return ""
}

// TestAFailedPollNamesItsResourceAndRevision: the failure surfaces in
// pollAndLog, above the resource's own scope, so the line has to be handed
// the resource back.
func TestAFailedPollNamesItsResourceAndRevision(t *testing.T) {
	dir := t.TempDir()
	cfg := loadConfig(t, dir, strings.Replace(pollFeed, "VERSIONS", filepath.Join(dir, "missing.json"), 1))
	st := mustOpenStore(t, dir)
	out := captureLog(t)

	(&admission{}).poll(t.Context(), cfg, st)

	line := logLine(out.String(), "trigger.poll")
	if line == "" {
		t.Fatalf("no trigger.poll line:\n%s", out)
	}

	for _, want := range []string{"resource=items", "revision=" + cfg.Revision.SHA} {
		if cfg.Revision.SHA == "" || !strings.Contains(line, want) {
			t.Errorf("trigger.poll lacks %s: %s", want, line)
		}
	}
}

// TestARefusedDeliveryNamesItsPipelineAndRevision: webhook lines come from a
// request, not the daemon's poll loop, so they are stamped where the request
// resolves its resource.
func TestARefusedDeliveryNamesItsPipelineAndRevision(t *testing.T) {
	f := newHookFixture(t, nil)
	f.cfg.Name = "app"
	f.cfg.Revision.SHA = "abc123"
	out := captureLog(t)
	body := []byte(`{"ref":"main"}`)

	if code := f.deliver(t, "push", signed("wrong", "d2", body), body); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}

	line := logLine(out.String(), "webhook.unauthorized")
	for _, want := range []string{"pipeline=app", "revision=abc123", "resource=push"} {
		if !strings.Contains(line, want) {
			t.Errorf("webhook.unauthorized lacks %s: %q", want, line)
		}
	}
}
