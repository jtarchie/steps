package trigger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
)

const hookSecret = "s3cret"

// TestRequestFromCarriesEveryPartOfTheDelivery: verifying and accepting both build their webhook.Request here, so a part dropped would be dropped from the signature check and the expression alike.
func TestRequestFromCarriesEveryPartOfTheDelivery(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/hooks/x?a=1&a=2", nil)
	r.Header.Set("X-Event", "push")

	got := requestFrom(r, []byte("body"))
	if got.Method != http.MethodPost || got.Header.Get("X-Event") != "push" || !reflect.DeepEqual(got.Query["a"], []string{"1", "2"}) || string(got.Body) != "body" {
		t.Errorf("requestFrom = %+v; want method, header, query and body all carried", got)
	}
}

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

	HookHandler(t.Context(), time.Hour, staticConfig(f.cfg), f.st)(rec, req, resource)

	return rec.Code
}

func (f hookFixture) response(t *testing.T, resource string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/p/test/hooks/"+resource, bytes.NewReader(nil))
	rec := httptest.NewRecorder()

	HookHandler(t.Context(), time.Hour, staticConfig(f.cfg), f.st)(rec, req, resource)

	return rec
}

func (f hookFixture) versions(t *testing.T) []store.VersionKey {
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

	if got := f.versions(t); len(got) != 1 || !strings.Contains(string(got[0]), `"d1"`) {
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

	out := captureLog(t)
	body := []byte(`{}`)

	if code := f.deliver(t, "push", signed("", "d1", body), body); code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}

	// The 401 alone sends an operator to the sender's signing; only this line names the variable that is actually empty.
	if line := logLine(out.String(), "webhook.secret_unset"); !strings.Contains(line, "env=STEPS_TEST_HOOK_SECRET") {
		t.Errorf("webhook.secret_unset = %q, want it to name the unset variable", line)
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

// TestAHookWithoutWebhookResourcesAnswersAsAnUnknownResource: a caller must not learn whether a pipeline has any webhook resources, so the answer is byte-identical to the unknown-resource one.
func TestAHookWithoutWebhookResourcesAnswersAsAnUnknownResource(t *testing.T) {
	with := newHookFixture(t, nil)
	without := newHookFixture(t, nil)
	without.cfg.Resources = without.cfg.Resources[1:]

	want := with.response(t, "nope")
	got := without.response(t, "push")

	if want.Code != http.StatusUnauthorized {
		t.Fatalf("unknown resource status = %d, want 401", want.Code)
	}

	if got.Code != want.Code || got.Body.String() != want.Body.String() || !reflect.DeepEqual(got.Header(), want.Header()) {
		t.Errorf("no webhook resources answered %d %q %v, want %d %q %v", got.Code, got.Body, got.Header(), want.Code, want.Body, want.Header())
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

	_, _, err := f.st.ClaimNextJob(context.Background())
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
	HookHandler(t.Context(), time.Hour, staticConfig(f.cfg), f.st)(rec, req, "push")

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

// racingStore records a second delivery the moment a poll has read the webhook's versions: the window between observing a delivery resource and recording its checked version.
type racingStore struct {
	store.Store

	race func()
}

func (r *racingStore) ResourceVersionsJSON(ctx context.Context, name string) ([]store.VersionKey, error) {
	versions, err := r.Store.ResourceVersionsJSON(ctx, name)

	if r.race != nil {
		race := r.race
		r.race = nil
		race()
	}

	return versions, err //nolint:wrapcheck // test passthrough
}

// TestAPollDoesNotRewindADeliveryItRaced: a delivery recorded mid-poll dispatched itself; a poll that then wrote back the version it read earlier would rewind the checked version, and the next poll would build that delivery a second time.
func TestAPollDoesNotRewindADeliveryItRaced(t *testing.T) {
	f := newHookFixture(t, nil)
	ctx := context.Background()

	body := []byte(`{}`)
	f.deliver(t, "push", signed(hookSecret, "d1", body), body)

	racing := &racingStore{Store: f.st}
	racing.race = func() { f.deliver(t, "push", signed(hookSecret, "d2", body), body) }

	_, err := pollOnce(ctx, f.cfg, racing)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = f.st.ClaimNextJob(ctx)
	if err != nil {
		t.Fatal(err)
	}

	enqueued, err := pollOnce(ctx, f.cfg, f.st)
	if err != nil || len(enqueued) != 0 {
		t.Errorf("the poll after a raced delivery enqueued %v (err %v), want nothing: d2 dispatched itself", enqueued, err)
	}
}

// TestAPollAfterUnpauseDoesNotRewindADeliveryItRaced: the poll dispatching a delivery held by a pause must not then overwrite the checked version a delivery recorded mid-poll set — otherwise the next poll dispatches that one again, and once its build has started the queue no longer absorbs it.
func TestAPollAfterUnpauseDoesNotRewindADeliveryItRaced(t *testing.T) {
	f := newHookFixture(t, nil)
	ctx := context.Background()
	body := []byte(`{}`)

	err := f.st.Pause(ctx)
	if err != nil {
		t.Fatal(err)
	}

	f.deliver(t, "push", signed(hookSecret, "d1", body), body)

	err = f.st.Unpause(ctx)
	if err != nil {
		t.Fatal(err)
	}

	racing := &racingStore{Store: f.st}
	racing.race = func() { f.deliver(t, "push", signed(hookSecret, "d2", body), body) }

	enqueued, err := pollOnce(ctx, f.cfg, racing)
	if err != nil || strings.Join(enqueued, ",") != "build" {
		t.Fatalf("the poll after unpause enqueued %v (err %v), want build", enqueued, err)
	}

	_, _, err = f.st.ClaimNextJob(ctx)
	if err != nil {
		t.Fatal(err)
	}

	enqueued, err = pollOnce(ctx, f.cfg, f.st)
	if err != nil || len(enqueued) != 0 {
		t.Errorf("the next poll enqueued %v (err %v), want nothing: d2 dispatched itself", enqueued, err)
	}
}

// vaultedHook is a webhook resource whose secret_env names a key of its secrets: map, under a context whose lookup the test scripts and counts.
type vaultedHook struct {
	hookFixture

	lookups atomic.Int32
	handler func(w http.ResponseWriter, r *http.Request, resource string)
}

func newVaultedHook(t *testing.T, ttl time.Duration, lookup func() (string, error)) *vaultedHook {
	t.Helper()

	// Unset, so a handler that fell back to the environment verifies against nothing.
	t.Setenv("STEPS_TEST_HOOK_SECRET", "")

	cfg := hookConfig(nil)
	cfg.Resources[0].Secrets = map[string]string{"STEPS_TEST_HOOK_SECRET": "((ci:hook_secret))"}

	hook := &vaultedHook{hookFixture: hookFixture{cfg: cfg, st: mustOpenStore(t, t.TempDir())}}

	base := rsrc.WithSecretLookup(t.Context(), func(_ context.Context, resource string, secrets map[string]string) (map[string]string, error) {
		hook.lookups.Add(1)

		if resource != "push" || secrets["STEPS_TEST_HOOK_SECRET"] != "((ci:hook_secret))" {
			t.Errorf("lookup for %q of %v, want the push resource's one reference", resource, secrets)
		}

		value, err := lookup()

		return map[string]string{"STEPS_TEST_HOOK_SECRET": value}, err
	})

	hook.handler = HookHandler(base, ttl, staticConfig(cfg), hook.st)

	return hook
}

func (h *vaultedHook) post(ctx context.Context, header http.Header, body []byte) int {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/p/test/hooks/push", bytes.NewReader(body))
	req.Header = header
	rec := httptest.NewRecorder()

	h.handler(rec, req, "push")

	return rec.Code
}

// The seam: the secret the delivery is verified against is the one the pipeline's lookup returned, fetched once however many deliveries arrive within the ttl.
func TestAHookVerifiesAgainstASecretFromASource(t *testing.T) {
	hook := newVaultedHook(t, time.Hour, func() (string, error) { return hookSecret, nil })
	body := []byte(`{"ref":"main"}`)

	if code := hook.post(t.Context(), signed(hookSecret, "d1", body), body); code != http.StatusOK {
		t.Fatalf("a delivery signed with the source's secret answered %d, want 200", code)
	}

	if code := hook.post(t.Context(), signed("wrong", "d2", body), body); code != http.StatusUnauthorized {
		t.Errorf("a delivery signed with another secret answered %d, want 401", code)
	}

	if got := hook.versions(t); len(got) != 1 {
		t.Errorf("versions = %v, want the one verified delivery", got)
	}

	if got := hook.lookups.Load(); got != 1 {
		t.Errorf("the source was asked %d times for two deliveries inside the ttl, want once", got)
	}
}

// The lookup runs before the signature is checked, so anyone can cause one: a failing source must be asked once per ttl, not once per POST, and no delivery may be verified against nothing meanwhile.
func TestAFailingSecretLookupIsAskedOncePerTTL(t *testing.T) {
	hook := newVaultedHook(t, time.Hour, func() (string, error) { return "", errors.New("the vault is locked") })
	body := []byte(`{}`)

	for range 50 {
		// Signed with the empty key: what an unset secret would have verified, were the failure read as one.
		if code := hook.post(t.Context(), signed("", "d1", body), body); code != http.StatusServiceUnavailable {
			t.Fatalf("a delivery while the secret cannot be fetched answered %d, want 503", code)
		}
	}

	if got := hook.lookups.Load(); got != 1 {
		t.Errorf("the failing source was asked %d times by 50 deliveries, want once", got)
	}

	if got := hook.versions(t); len(got) != 0 {
		t.Errorf("versions = %v, want nothing recorded while the secret is unavailable", got)
	}
}

// A sender that hangs up while the source is still answering gets a 503, and the lookup it started is the one the next delivery is verified with.
func TestASenderThatHangsUpDoesNotCostASecondLookup(t *testing.T) {
	release := make(chan struct{})
	hook := newVaultedHook(t, time.Hour, func() (string, error) {
		<-release

		return hookSecret, nil
	})
	body := []byte(`{"ref":"main"}`)

	impatient, hangUp := context.WithCancel(t.Context())
	hangUp()

	if code := hook.post(impatient, signed(hookSecret, "d1", body), body); code != http.StatusServiceUnavailable {
		t.Fatalf("a delivery abandoned mid-lookup answered %d, want 503", code)
	}

	close(release)

	if code := hook.post(t.Context(), signed(hookSecret, "d1", body), body); code != http.StatusOK {
		t.Fatalf("the redelivery answered %d, want 200", code)
	}

	if got := hook.lookups.Load(); got != 1 {
		t.Errorf("the source was asked %d times, want the one lookup the first delivery started", got)
	}
}

// A rotated secret reaches the handler once the ttl has passed, and a failure is forgotten on the same clock.
func TestALookedUpSecretIsAskedForAgainAfterTheTTL(t *testing.T) {
	var answers atomic.Int32

	hook := newVaultedHook(t, time.Millisecond, func() (string, error) {
		if answers.Add(1) == 1 {
			return "", errors.New("the vault is locked")
		}

		return hookSecret, nil
	})
	body := []byte(`{"ref":"main"}`)

	if code := hook.post(t.Context(), signed(hookSecret, "d1", body), body); code != http.StatusServiceUnavailable {
		t.Fatalf("the first delivery answered %d, want 503", code)
	}

	time.Sleep(20 * time.Millisecond)

	if code := hook.post(t.Context(), signed(hookSecret, "d1", body), body); code != http.StatusOK {
		t.Errorf("a delivery after the ttl answered %d, want the source asked again and 200", code)
	}
}
