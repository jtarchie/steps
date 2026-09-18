package trigger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

const hookSecret = "s3cret"

func hookConfig(source map[string]any) *config.Config {
	merged := map[string]any{"provider": "github", "secret_env": "STEPS_TEST_HOOK_SECRET"}
	for key, value := range source {
		merged[key] = value
	}

	return &config.Config{
		Resources: []config.Resource{
			{Name: "push", Type: config.WebhookType, Source: merged},
			{Name: "other", Type: "git", Source: map[string]any{}},
		},
		Jobs: []config.Job{{
			Name: "build",
			Plan: []config.Step{
				{Get: "push", Trigger: true},
				{Task: "work", Run: "true", Inputs: config.Inputs()},
			},
		}},
	}
}

type hookFixture struct {
	cfg *config.Config
	st  store.Store
}

func newHookFixture(t *testing.T, source map[string]any) hookFixture {
	t.Helper()
	t.Setenv("STEPS_TEST_HOOK_SECRET", hookSecret)

	return hookFixture{cfg: hookConfig(source), st: mustOpenStore(t, t.TempDir())}
}

func signed(secret, id string, body []byte) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return http.Header{
		"X-Github-Event":      {"push"},
		"X-Github-Delivery":   {id},
		"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))},
	}
}

func (f hookFixture) deliver(t *testing.T, resource string, header http.Header, body []byte) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/p/test/hooks/"+resource, bytes.NewReader(body))
	req.Header = header
	rec := httptest.NewRecorder()

	HookHandler(staticConfig(f.cfg), f.st)(rec, req, resource)

	return rec.Code
}

func (f hookFixture) versions(t *testing.T) []string {
	t.Helper()

	versions, err := f.st.ResourceVersionsJSON(context.Background(), "push")
	if err != nil {
		t.Fatal(err)
	}

	return versions
}

func (f hookFixture) pending(t *testing.T) []string {
	t.Helper()

	rows, err := f.st.ListTriggerQueue(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}

	var pending []string

	for _, row := range rows {
		if row.Status == "pending" {
			pending = append(pending, row.JobName)
		}
	}

	return pending
}

func TestAHookRecordsAndQueues(t *testing.T) {
	f := newHookFixture(t, nil)
	body := []byte(`{"ref":"main"}`)

	if code := f.deliver(t, "push", signed(hookSecret, "d1", body), body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	if got := f.versions(t); len(got) != 1 || !strings.Contains(got[0], `"d1"`) {
		t.Errorf("versions = %v, want the delivery", got)
	}

	if got := f.pending(t); strings.Join(got, ",") != "build" {
		t.Errorf("pending = %v, want build", got)
	}
}

func TestAHookRefusesABadSignature(t *testing.T) {
	f := newHookFixture(t, nil)
	body := []byte(`{"ref":"main"}`)

	if code := f.deliver(t, "push", signed("wrong", "d2", body), body); code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}

	if got := f.versions(t); len(got) != 0 {
		t.Errorf("versions = %v after a bad signature, want nothing recorded", got)
	}
}

// TestAHookWithAnUnsetSecretRefusesEverything: an empty secret is a misconfiguration, and reading it as "no auth" would open the endpoint — including to a delivery signed with the empty key.
func TestAHookWithAnUnsetSecretRefusesEverything(t *testing.T) {
	f := newHookFixture(t, nil)
	t.Setenv("STEPS_TEST_HOOK_SECRET", "")

	body := []byte(`{}`)

	if code := f.deliver(t, "push", signed("", "d1", body), body); code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// TestAHookDoesNotEnumerateResources: an unknown resource and one that is not a webhook answer exactly as a bad signature does.
func TestAHookDoesNotEnumerateResources(t *testing.T) {
	f := newHookFixture(t, nil)
	body := []byte(`{}`)

	for _, name := range []string{"nope", "other"} {
		if code := f.deliver(t, name, signed(hookSecret, "d1", body), body); code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, code)
		}
	}
}

func TestAHookIsAbsentWithoutWebhookResources(t *testing.T) {
	f := newHookFixture(t, nil)
	f.cfg.Resources = f.cfg.Resources[1:]

	if code := f.deliver(t, "push", http.Header{}, nil); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestAHookRefusesAnOversizedBody(t *testing.T) {
	f := newHookFixture(t, map[string]any{"max_body": 8})
	body := []byte(`{"ref":"refs/heads/main"}`)

	if code := f.deliver(t, "push", signed(hookSecret, "d1", body), body); code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", code)
	}
}

// TestARedeliveredHookQueuesNothing: GitHub's redeliver button resends the same X-GitHub-Delivery, which must not build twice.
func TestARedeliveredHookQueuesNothing(t *testing.T) {
	f := newHookFixture(t, nil)
	body := []byte(`{}`)

	f.deliver(t, "push", signed(hookSecret, "d1", body), body)

	_, _, _, err := f.st.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if code := f.deliver(t, "push", signed(hookSecret, "d1", body), body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the sender did nothing wrong", code)
	}

	if got := f.pending(t); len(got) != 0 {
		t.Errorf("pending = %v after a redelivery, want nothing", got)
	}
}

// TestAPausedHookBuildsOnUnpause: a paused pipeline keeps the delivery and queues nothing, and the first poll after unpause queues it — answering 200 and dropping it would be a loss the sender could not redeliver, since it thinks it succeeded.
func TestAPausedHookBuildsOnUnpause(t *testing.T) {
	f := newHookFixture(t, nil)
	ctx := context.Background()

	err := f.st.Pause(ctx)
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{}`)
	if code := f.deliver(t, "push", signed(hookSecret, "d1", body), body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	if got := f.pending(t); len(got) != 0 {
		t.Errorf("pending = %v while paused, want nothing", got)
	}

	err = f.st.Unpause(ctx)
	if err != nil {
		t.Fatal(err)
	}

	enqueued, err := pollOnce(ctx, f.cfg, f.st)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(enqueued, ",") != "build" {
		t.Errorf("the poll after unpause enqueued %v, want build", enqueued)
	}

	enqueued, err = pollOnce(ctx, f.cfg, f.st)
	if err != nil || len(enqueued) != 0 {
		t.Errorf("a second poll enqueued %v (err %v), want nothing: the delivery was dispatched once", enqueued, err)
	}
}

// TestASlackHandshakeIsAnsweredAndNotRecorded: Slack will not save an Events API URL until the challenge comes back, and the challenge is not an event anything should build.
func TestASlackHandshakeIsAnsweredAndNotRecorded(t *testing.T) {
	f := newHookFixture(t, map[string]any{"provider": "slack"})
	body := []byte(`{"type":"url_verification","challenge":"c4allenge"}`)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)

	mac := hmac.New(sha256.New, []byte(hookSecret))
	mac.Write([]byte("v0:" + stamp + ":"))
	mac.Write(body)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/p/test/hooks/push", bytes.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	HookHandler(staticConfig(f.cfg), f.st)(rec, req, "push")

	if rec.Code != http.StatusOK || rec.Body.String() != "c4allenge" {
		t.Errorf("answered %d %q, want 200 with the challenge", rec.Code, rec.Body.String())
	}

	if got := f.versions(t); len(got) != 0 {
		t.Errorf("versions = %v, want the handshake unrecorded", got)
	}
}

// TestAnExpressionThatFailsIsA500: the pipeline's mistake, not the sender's — so the sender logs a failure it can redeliver once the pipeline is fixed, and nothing is recorded meanwhile.
func TestAnExpressionThatFailsIsA500(t *testing.T) {
	f := newHookFixture(t, map[string]any{"id": `payload.head.sha`})
	body := []byte(`{"head":null}`)

	if code := f.deliver(t, "push", signed(hookSecret, "d3", body), body); code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", code)
	}

	if got := f.versions(t); len(got) != 0 {
		t.Errorf("versions = %v, want nothing recorded", got)
	}
}
