package e2e

// type: webhook — a delivery that reaches the daemon IS a version, and the job it triggers reads the payload.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

const githubSecret = "It's a Secret to Everybody"

func deliveryPipeline(out string) string {
	return `
resources:
- name: push
  type: webhook
  source:
    provider: github
    secret_env: STEPS_TEST_GITHUB_SECRET
jobs:
- name: build
  plan:
  - get: push
    trigger: true
  - task: build
    inputs: [push]
    run: |
      mkdir -p ` + out + `
      cp push/body push/version.json push/headers.json ` + out + `/
`
}

// githubDelivery is what GitHub sends: the body signed with the shared secret in X-Hub-Signature-256, the event and a delivery id in headers of their own.
func githubDelivery(secret, event, id string, body []byte) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return http.Header{
		"Content-Type":        {"application/json"},
		"X-GitHub-Event":      {event},
		"X-GitHub-Delivery":   {id},
		"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))},
	}
}

// postEmpty POSTs nothing, the way a UI control or a sender with no body does, and returns the status.
func postEmpty(t *testing.T, url string) int {
	t.Helper()

	return postDelivery(t, url, http.Header{}, nil)
}

// postDelivery sends one webhook delivery and returns its status; keep-alives off and the body drained, because a pooled connection is one http.Server.Shutdown waits its whole grace period for.
func postDelivery(t *testing.T, url string, header http.Header, body []byte) int {
	t.Helper()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header = header

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode
}

func TestWebhookDeliveryIsTheVersionTheJobBuilds(t *testing.T) {
	t.Setenv("STEPS_TEST_GITHUB_SECRET", githubSecret)

	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	path := writePipeline(t, dir, deliveryPipeline(out))

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	body := []byte(`{"ref":"refs/heads/main","after":"6113728f27ae82c7b1a177c8d03f9e96e0adf246"}`)
	url := fmt.Sprintf("http://%s/p/%s/hooks/push", served.addr, cli.PipelineName(path))

	status := postDelivery(t, url, githubDelivery(githubSecret, "push", "72d3162e-cc78-11e3-81ab-4c9367dc0958", body), body)
	if status != http.StatusOK {
		t.Fatalf("delivery answered %d, want 200", status)
	}

	waitForFile(t, filepath.Join(out, "headers.json"))

	if got := readFileString(t, filepath.Join(out, "body")); got != string(body) {
		t.Errorf("push/body = %s, want the delivery's body byte for byte", got)
	}

	var version map[string]string

	err := json.Unmarshal([]byte(readFileString(t, filepath.Join(out, "version.json"))), &version)
	if err != nil {
		t.Fatalf("version.json: %v", err)
	}

	if version["id"] != "72d3162e-cc78-11e3-81ab-4c9367dc0958" || version["event"] != "push" {
		t.Errorf("version = %v, want the delivery id and event", version)
	}

	headers := readFileString(t, filepath.Join(out, "headers.json"))
	if strings.Contains(headers, "x-hub-signature-256") || !strings.Contains(headers, "x-github-event") {
		t.Errorf("headers.json = %s, want the event header kept and the signature stripped", headers)
	}
}

// TestWebhookDeliveryWorksUnderReadOnly: --read-only withholds the browser's controls, not a sender's signed delivery — a read-only box that could not be notified is most of what a read-only build box is for.
func TestWebhookDeliveryWorksUnderReadOnly(t *testing.T) {
	t.Setenv("STEPS_TEST_GITHUB_SECRET", githubSecret)

	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	path := writePipeline(t, dir, deliveryPipeline(out))

	served := startWebFor(t, path, "--read-only", "--interval", "1h")
	defer served.stop(t)

	name := cli.PipelineName(path)

	trigger := fmt.Sprintf("http://%s/p/%s/jobs/build/trigger", served.addr, name)
	if status := postEmpty(t, trigger); status != http.StatusForbidden {
		t.Errorf("the UI trigger answered %d under --read-only, want 403", status)
	}

	body := []byte(`{}`)
	url := fmt.Sprintf("http://%s/p/%s/hooks/push", served.addr, name)

	if status := postDelivery(t, url, githubDelivery("wrong", "push", "d1", body), body); status != http.StatusUnauthorized {
		t.Errorf("a bad signature answered %d under --read-only, want 401", status)
	}

	// An Origin a sender chose, which the UI's same-origin check would refuse: a delivery is authenticated by its signature instead.
	signedDelivery := githubDelivery(githubSecret, "push", "d1", body)
	signedDelivery.Set("Origin", "https://sender.example")

	if status := postDelivery(t, url, signedDelivery, body); status != http.StatusOK {
		t.Fatalf("delivery answered %d under --read-only, want 200", status)
	}

	waitForFile(t, filepath.Join(out, "headers.json"))
}

// TestWebhookExpressionsAreThePipelines: filter: keeps only what the pipeline cares about, id: makes the same commit one version however many deliveries carry it, and version: puts what matters where the UI shows it.
func TestWebhookExpressionsAreThePipelines(t *testing.T) {
	t.Setenv("STEPS_TEST_GITHUB_SECRET", githubSecret)

	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	path := writePipeline(t, dir, `
resources:
- name: push
  type: webhook
  source:
    provider: github
    secret_env: STEPS_TEST_GITHUB_SECRET
    filter: 'event == "push" && payload.ref == "refs/heads/main"'
    id: 'payload.after'
    version:
      branch: 'trimPrefix(payload.ref, "refs/heads/")'
jobs:
- name: build
  plan:
  - get: push
    trigger: true
  - task: build
    inputs: [push]
    run: |
      mkdir -p `+out+`
      cp push/version.json `+out+`/
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	name := cli.PipelineName(path)
	url := fmt.Sprintf("http://%s/p/%s/hooks/push", served.addr, name)

	deliveries := []struct {
		event, id, body string
	}{
		{"ping", "d1", `{"zen":"Keep it logically awesome."}`},
		{"push", "d2", `{"ref":"refs/heads/feature","after":"bbb"}`},
		{"push", "d3", `{"ref":"refs/heads/main","after":"aaa"}`},
		{"push", "d4", `{"ref":"refs/heads/main","after":"aaa"}`},
	}

	for _, d := range deliveries {
		body := []byte(d.body)
		if status := postDelivery(t, url, githubDelivery(githubSecret, d.event, d.id, body), body); status != http.StatusOK {
			t.Fatalf("delivery %s answered %d, want 200: a filtered delivery and a repeat are both the sender doing nothing wrong", d.id, status)
		}
	}

	st := waitForStore(t, served.state, name)
	defer func() { _ = st.Close() }()

	versions, err := st.ResourceVersionsJSON(t.Context(), "push")
	if err != nil {
		t.Fatal(err)
	}

	want := `{"branch":"main","event":"push","id":"aaa"}`
	if len(versions) != 1 || versions[0] != want {
		t.Errorf("versions = %v, want only %s: the ping and the feature push filtered out, the second delivery of aaa the same version", versions, want)
	}

	waitForFile(t, filepath.Join(out, "version.json"))
}

// TestRunDeliversACapturedRequest: a local command has no daemon to POST to, so --deliver hands it a captured request — through the pipeline's own filter:, id: and version:, with only the signature skipped, which is where a pipeline's mistakes live.
func TestRunDeliversACapturedRequest(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	path := writePipeline(t, dir, `
resources:
- name: push
  type: webhook
  source:
    provider: github
    secret_env: STEPS_TEST_UNSET_SECRET
    filter: 'event == "push"'
    version:
      sha: 'payload.after'
jobs:
- name: build
  plan:
  - get: push
  - task: build
    inputs: [push]
    run: |
      mkdir -p `+out+`
      cp push/body push/version.json `+out+`/
`)

	captured := filepath.Join(dir, "push.http")
	writeCaptured(t, captured, "POST /p/app/hooks/push HTTP/1.1\nX-GitHub-Event: push\nX-GitHub-Delivery: d1\nContent-Type: application/json\n\n{\"after\":\"aaa\"}\n")

	err := cli.Run([]string{"run", path, "--deliver", "push=" + captured})
	if err != nil {
		t.Fatalf("steps run --deliver: %v", err)
	}

	if got := readFileString(t, filepath.Join(out, "version.json")); got != `{"event":"push","id":"d1","sha":"aaa"}` {
		t.Errorf("version.json = %s, want the delivery projected by the pipeline's own expressions", got)
	}

	if got := readFileString(t, filepath.Join(out, "body")); got != "{\"after\":\"aaa\"}\n" {
		t.Errorf("body = %q, want everything after the blank line", got)
	}

	ping := filepath.Join(dir, "ping.http")
	writeCaptured(t, ping, "POST / HTTP/1.1\nX-GitHub-Event: ping\nX-GitHub-Delivery: d2\n\n{}")

	err = cli.Run([]string{"run", path, "--deliver", "push=" + ping})
	if err == nil || !strings.Contains(err.Error(), "filter") {
		t.Errorf("err = %v, want the filtered delivery named rather than the last version silently rebuilt", err)
	}
}

func writeCaptured(t *testing.T, path, request string) {
	t.Helper()

	err := os.WriteFile(path, []byte(request), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// TestWebhookDeliveryWhilePausedBuildsOnUnpause: a paused pipeline records the delivery and queues nothing, and unpausing builds it — answering 200 and dropping it would be a loss the sender could never redeliver, since it believes it succeeded.
func TestWebhookDeliveryWhilePausedBuildsOnUnpause(t *testing.T) {
	t.Setenv("STEPS_TEST_GITHUB_SECRET", githubSecret)

	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	path := writePipeline(t, dir, deliveryPipeline(out))

	served := startWebFor(t, path, "--interval", "100ms")
	defer served.stop(t)

	name := cli.PipelineName(path)
	served.pipeline(t, "pause", "-p", name)

	body := []byte(`{}`)
	url := fmt.Sprintf("http://%s/p/%s/hooks/push", served.addr, name)

	if status := postDelivery(t, url, githubDelivery(githubSecret, "push", "d1", body), body); status != http.StatusOK {
		t.Fatalf("delivery to a paused pipeline answered %d, want 200", status)
	}

	time.Sleep(drainIdleWindow)
	assertNoFile(t, filepath.Join(out, "version.json"))

	served.pipeline(t, "unpause", "-p", name)
	waitForFile(t, filepath.Join(out, "version.json"))
}
