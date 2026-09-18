package webhook

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture is one delivery a provider sends, with where it came from: a vector the sender publishes where one exists, since a delivery built from this package's own reading of the docs would pass whatever that reading got wrong.
type fixture struct {
	Provenance string            `json:"provenance"`
	Secret     string            `json:"secret"`
	Now        int64             `json:"now"`
	Body       string            `json:"body"`
	Headers    map[string]string `json:"headers"`
	ID         string            `json:"id"`
	Event      string            `json:"event"`
	// PresentsSecret marks a sender that sends the secret itself rather than signing the body, so changing the body cannot be what fails it.
	PresentsSecret bool `json:"presents_secret"`
}

func loadFixture(t *testing.T, name string) (fixture, Request, time.Time) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name+".json")) //nolint:gosec // name is a provider table key, read from this package's own testdata
	if err != nil {
		t.Fatalf("provider %q has no fixture: every provider carries a delivery that verifies, in testdata/%s.json", name, name)
	}

	var f fixture

	err = json.Unmarshal(raw, &f)
	if err != nil {
		t.Fatal(err)
	}

	header := http.Header{}
	for key, value := range f.Headers {
		header.Set(key, value)
	}

	now := time.Now()
	if f.Now != 0 {
		now = time.Unix(f.Now, 0)
	}

	return f, Request{Header: header, Body: []byte(f.Body)}, now
}

func verifies(name string, req Request, secret string, now time.Time) bool {
	return (&Receiver{Provider: name}).Verify(req.Header, req.Body, secret, now) == nil
}

func flipped(value string) string {
	b := []byte(value)
	b[len(b)-1] ^= 1

	return string(b)
}

// TestEveryProviderHasAFixture holds the table to what a provider must prove: a delivery that verifies, stops verifying when one byte changes, and never verifies with an empty secret. A provider added without one fails here.
func TestEveryProviderHasAFixture(t *testing.T) {
	for name, provider := range Providers {
		t.Run(name, func(t *testing.T) {
			f, req, now := loadFixture(t, name)

			if !verifies(name, req, f.Secret, now) {
				t.Fatalf("the fixture does not verify (%s)", f.Provenance)
			}

			if got := provider.ID(req); got != f.ID {
				t.Errorf("id = %q, want %q", got, f.ID)
			}

			if got := provider.Event(req); got != f.Event {
				t.Errorf("event = %q, want %q", got, f.Event)
			}

			if verifies(name, req, "", now) {
				t.Error("verified with an empty secret")
			}

			if !f.PresentsSecret {
				changed := Request{Header: req.Header, Body: []byte(flipped(f.Body))}
				if verifies(name, changed, f.Secret, now) {
					t.Error("verified with one byte of the body changed")
				}
			}

			credential := provider.Signed[0]
			changed := req.Header.Clone()
			changed.Set(credential, flipped(req.Header.Get(credential)))

			if verifies(name, Request{Header: changed, Body: req.Body}, f.Secret, now) {
				t.Errorf("verified with one byte of %s changed", credential)
			}
		})
	}
}

// TestTimestampedProvidersRefuseAStaleDelivery: a signed timestamp nobody reads lets a captured delivery replay forever.
func TestTimestampedProvidersRefuseAStaleDelivery(t *testing.T) {
	for _, name := range []string{"slack", "stripe", "standard-webhooks"} {
		t.Run(name, func(t *testing.T) {
			f, req, now := loadFixture(t, name)

			for _, skew := range []time.Duration{Tolerance + time.Second, -Tolerance - time.Second} {
				if verifies(name, req, f.Secret, now.Add(skew)) {
					t.Errorf("verified %s away from its timestamp", skew)
				}
			}

			if !verifies(name, req, f.Secret, now.Add(Tolerance-time.Second)) {
				t.Error("refused a delivery inside the tolerance")
			}
		})
	}
}

// TestEveryFixtureNamesAProvider: a fixture whose provider was renamed or removed is a test that silently stopped running.
func TestEveryFixtureNamesAProvider(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		if _, ok := Providers[name]; !ok {
			t.Errorf("testdata/%s.json names no provider", name)
		}
	}
}

// TestSlackAnswersItsURLVerification: Slack will not save an Events API URL until it echoes the challenge — signed like any delivery, so it is verified first.
func TestSlackAnswersItsURLVerification(t *testing.T) {
	body := []byte(`{"token":"x","challenge":"3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P","type":"url_verification"}`)

	result, err := (&Receiver{Provider: "slack"}).Accept(http.MethodPost, http.Header{}, nil, body)
	if err != nil {
		t.Fatal(err)
	}

	if string(result.Handshake) != "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" {
		t.Errorf("handshake = %q, want the challenge echoed", result.Handshake)
	}
}
