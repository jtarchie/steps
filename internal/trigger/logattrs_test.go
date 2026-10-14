package trigger

// A daemon polls several pipelines at once, so a line that says a check failed
// is only useful if it says whose.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
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

type refusingVersions struct{ store.Store }

func (refusingVersions) RecordCheckedVersion(context.Context, string, store.VersionKey) error {
	return os.ErrPermission
}

// TestAnUnrecordedVersionNamesItsResource: advancing a resource fails after
// every check has run, so it is not a check's failure and still has a
// resource to name.
func TestAnUnrecordedVersionNamesItsResource(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	writeVersions(t, versions, `[{"n":"1"}]`)

	cfg := loadConfig(t, dir, strings.Replace(pollFeed, "VERSIONS", versions, 1))
	st := mustOpenStore(t, dir)

	// A cold start records its baseline inside the check; only a later poll reaches advance.
	_, err := pollOnce(t.Context(), cfg, st)
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}

	writeVersions(t, versions, `[{"n":"1"},{"n":"2"}]`)

	out := captureLog(t)

	pollAndLog(t.Context(), cfg, refusingVersions{st})

	line := logLine(out.String(), "trigger.poll")
	if !strings.Contains(line, "record version") || !strings.Contains(line, "resource=items") {
		t.Errorf("trigger.poll lacks the unrecorded resource: %q", line)
	}
}

type refusingCheckErrors struct{ store.Store }

func (refusingCheckErrors) RecordCheckError(context.Context, string, string) error {
	return os.ErrPermission
}

// TestAnUnrecordedCheckOutcomeIsLogged: the outcome is a diagnostic and its own failure is not returned, so the log is the only place it can show.
func TestAnUnrecordedCheckOutcomeIsLogged(t *testing.T) {
	dir := t.TempDir()
	cfg := loadConfig(t, dir, strings.Replace(pollFeed, "VERSIONS", filepath.Join(dir, "missing.json"), 1))
	st := mustOpenStore(t, dir)
	out := captureLog(t)

	_, _ = pollOnce(t.Context(), cfg, refusingCheckErrors{st})

	line := logLine(out.String(), "trigger.check_error_record")
	if !strings.Contains(line, "resource=items") || !strings.Contains(line, os.ErrPermission.Error()) {
		t.Errorf("trigger.check_error_record = %q, want the resource and the store's refusal", line)
	}
}

const twoBrokenFeeds = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: broken
  config: {check: "exit 1"}
resources:
- {name: first, type: broken, source: {}}
- {name: second, type: broken, source: {}}
jobs:
- name: a
  plan: [{get: first, trigger: true}]
- name: b
  plan: [{get: second, trigger: true}]
`

// TestEveryFailingCheckGetsItsOwnLine: a poll carries on past a failing check,
// so several can fail at once, and each line must still say whose it is.
func TestEveryFailingCheckGetsItsOwnLine(t *testing.T) {
	dir := t.TempDir()
	cfg := loadConfig(t, dir, twoBrokenFeeds)
	st := mustOpenStore(t, dir)
	out := captureLog(t)

	pollAndLog(t.Context(), cfg, st)

	var named []string

	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "msg=trigger.poll ") {
			named = append(named, line)
		}
	}

	if len(named) != 2 || !strings.Contains(named[0], "resource=first") || !strings.Contains(named[1], "resource=second") {
		t.Errorf("trigger.poll lines = %q, want one naming each failing resource", named)
	}
}

// TestAShutdownIsOneLineNotOnePerResource: cancelling the daemon fails every
// check still to run, and reporting each would bury the one fact — it stopped.
func TestAShutdownIsOneLineNotOnePerResource(t *testing.T) {
	dir := t.TempDir()
	cfg := loadConfig(t, dir, twoBrokenFeeds)
	st := mustOpenStore(t, dir)
	out := captureLog(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	pollAndLog(ctx, cfg, st)

	if got := strings.Count(out.String(), "msg=trigger.poll "); got != 1 {
		t.Errorf("a cancelled poll logged %d trigger.poll lines, want 1:\n%s", got, out)
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

// probedStore logs from inside the store call, so a line logged below the
// handler — jobReadyFor's waiting_on_passed — is judged on the ctx it gets.
type probedStore struct {
	HookStore
}

func (p probedStore) Paused(ctx context.Context) (bool, error) {
	slog.InfoContext(ctx, "probe.paused")

	return p.HookStore.Paused(ctx) //nolint:wrapcheck // a pass-through
}

// TestADeliveryDispatchesUnderItsIdentity: the dispatch below the handler
// reads the stamped ctx, not the request's bare one.
func TestADeliveryDispatchesUnderItsIdentity(t *testing.T) {
	f := newHookFixture(t, nil)
	f.cfg.Name = "app"
	f.cfg.Revision.SHA = "abc123"
	out := captureLog(t)
	body := []byte(`{"ref":"main"}`)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/p/app/hooks/push", bytes.NewReader(body))
	req.Header = signed(hookSecret, "d1", body)
	rec := httptest.NewRecorder()

	HookHandler(staticConfig(f.cfg), probedStore{f.st})(rec, req, "push")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	line := logLine(out.String(), "probe.paused")
	for _, want := range []string{"pipeline=app", "revision=abc123", "resource=push"} {
		if !strings.Contains(line, want) {
			t.Errorf("dispatch ctx lacks %s: %q", want, line)
		}
	}
}

// TestAMisconfiguredHookNamesItsRevision: the receiver failing to build is
// logged before the resource is stamped, but the pipeline and revision are
// the daemon's own and belong on the line regardless.
func TestAMisconfiguredHookNamesItsRevision(t *testing.T) {
	f := newHookFixture(t, map[string]any{"provider": "nope"})
	f.cfg.Name = "app"
	f.cfg.Revision.SHA = "abc123"
	out := captureLog(t)
	body := []byte(`{}`)

	if code := f.deliver(t, "push", signed(hookSecret, "d1", body), body); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}

	line := logLine(out.String(), "webhook.config")
	for _, want := range []string{"pipeline=app", "revision=abc123", "resource=push"} {
		if !strings.Contains(line, want) {
			t.Errorf("webhook.config lacks %s: %q", want, line)
		}
	}
}
