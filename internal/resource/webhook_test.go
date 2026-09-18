package resource

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/webhook"
)

// TestAnUnknownProviderIsRefusedAtLoad: the provider is declared, never detected, so a name the table does not hold is a mistake to refuse where the pipeline is set, not a scheme to guess at the first delivery.
func TestAnUnknownProviderIsRefusedAtLoad(t *testing.T) {
	cfg := &config.Config{Resources: []config.Resource{{
		Name: "push", Type: config.WebhookType,
		Source: map[string]any{"provider": "gihtub", "secret_env": "SECRET"},
	}}}

	err := CompileWebhooks(cfg)
	if err == nil || !strings.Contains(err.Error(), `unknown webhook provider "gihtub"`) {
		t.Errorf("err = %v, want the unknown provider named", err)
	}
}

func webhookResource(source map[string]any) config.Resource {
	merged := map[string]any{"provider": "github", "secret_env": "SECRET"}
	for key, value := range source {
		merged[key] = value
	}

	return config.Resource{Name: "push", Type: config.WebhookType, Source: merged}
}

// TestACustomSchemeIsCheckedFromItsDescription is the seam from YAML to Go: signed: and timestamp: are expressions that build strings, and the MAC over them is compared in Go.
func TestACustomSchemeIsCheckedFromItsDescription(t *testing.T) {
	receiver, err := Receiver(webhookResource(map[string]any{
		"provider": "custom",
		"signature": map[string]any{
			"header": "X-Sig", "prefix": "v1=", "encoding": "base64", "algorithm": "sha512",
			"signed": `headers["x-ts"] + "." + body`, "timestamp": `headers["x-ts"]`,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1700000000, 0)
	body := []byte(`{"ok":true}`)

	mac := hmac.New(sha512.New, []byte("s3cret"))
	mac.Write([]byte("1700000000." + string(body)))

	header := http.Header{"X-Ts": {"1700000000"}, "X-Sig": {"v1=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))}}
	req := webhook.Request{Method: http.MethodPost, Header: header, Body: body}

	err = receiver.Verify(req, "s3cret", now)
	if err != nil {
		t.Fatalf("a delivery signed as described did not verify: %v", err)
	}

	if receiver.Verify(req, "s3cret", now.Add(webhook.Tolerance+time.Second)) == nil {
		t.Error("verified outside the timestamp tolerance")
	}

	req.Body = []byte(`{"ok":false}`)
	if receiver.Verify(req, "s3cret", now) == nil {
		t.Error("verified a changed body")
	}
}

// TestEveryBadExpressionIsNamed: one set reports every mistake in a resource, each by the field it is in.
func TestEveryBadExpressionIsNamed(t *testing.T) {
	_, err := Receiver(webhookResource(map[string]any{
		"filter":  `event ==`,
		"id":      `payload.`,
		"version": map[string]any{"sha": `)`},
	}))

	for _, field := range []string{"source.filter", "source.id", "source.version.sha"} {
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("err = %v, want %s named", err, field)
		}
	}
}

// TestAVersionFieldIsAString: a JSON number decodes as float64, and a pull request id must not come back in exponent notation.
func TestAVersionFieldIsAString(t *testing.T) {
	receiver, err := Receiver(webhookResource(map[string]any{"version": map[string]any{"pr": `payload.number`}}))
	if err != nil {
		t.Fatal(err)
	}

	result, err := receiver.Accept(webhook.Request{Header: http.Header{}, Body: []byte(`{"number":3370172613}`)})
	if err != nil {
		t.Fatal(err)
	}

	if got := result.Delivery.Version["pr"]; got != "3370172613" {
		t.Errorf("pr = %v, want 3370172613", got)
	}
}
