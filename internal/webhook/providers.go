package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Tolerance is how far a signed timestamp may sit from now, either way. Without it a captured delivery replays forever, which is the hole pocketci left by signing the timestamp and never reading it.
const Tolerance = 5 * time.Minute

// Providers is every signature scheme a resource may declare. A provider is declared by the pipeline, never detected from headers the sender controls.
var Providers = map[string]Provider{
	"github": {
		Verify: hmacBody("X-Hub-Signature-256", "sha256="),
		ID:     header("X-GitHub-Delivery"),
		Event:  header("X-GitHub-Event"),
		Signed: []string{"X-Hub-Signature-256", "X-Hub-Signature"},
	},
	"gitlab": {
		Verify: token("X-Gitlab-Token", ""),
		ID:     header("X-Gitlab-Event-UUID"),
		Event:  header("X-Gitlab-Event"),
		Signed: []string{"X-Gitlab-Token"},
	},
	"gitea": {
		Verify: hmacBody("X-Gitea-Signature", ""),
		ID:     header("X-Gitea-Delivery"),
		Event:  header("X-Gitea-Event"),
		Signed: []string{"X-Gitea-Signature", "X-Gogs-Signature", "X-Hub-Signature-256", "X-Hub-Signature"},
	},
	"forgejo": {
		Verify: hmacBody("X-Forgejo-Signature", ""),
		ID:     header("X-Forgejo-Delivery"),
		Event:  header("X-Forgejo-Event"),
		Signed: []string{"X-Forgejo-Signature", "X-Gitea-Signature", "X-Gogs-Signature", "X-Hub-Signature-256", "X-Hub-Signature"},
	},
	"bitbucket": {
		Verify: hmacBody("X-Hub-Signature", "sha256="),
		ID:     header("X-Request-UUID"),
		Event:  header("X-Event-Key"),
		Signed: []string{"X-Hub-Signature"},
	},
	"slack": {
		Verify:    slackVerify,
		ID:        jsonField("event_id"),
		Event:     slackEvent,
		Handshake: slackHandshake,
		Signed:    []string{"X-Slack-Signature"},
	},
	"stripe": {
		Verify: stripeVerify,
		ID:     jsonField("id"),
		Event:  jsonField("type"),
		Signed: []string{"Stripe-Signature"},
	},
	"sentry": {
		Verify: hmacBody("Sentry-Hook-Signature", ""),
		ID:     header("Request-ID"),
		Event:  header("Sentry-Hook-Resource"),
		Signed: []string{"Sentry-Hook-Signature"},
	},
	"linear": {
		Verify: hmacBody("Linear-Signature", ""),
		ID:     header("Linear-Delivery"),
		Event:  header("Linear-Event"),
		Signed: []string{"Linear-Signature"},
	},
	"pagerduty": {
		Verify: pagerdutyVerify,
		ID:     jsonField("event", "id"),
		Event:  jsonField("event", "event_type"),
		Signed: []string{"X-PagerDuty-Signature"},
	},
	"honeybadger": {
		Verify: token("Honeybadger-Token", ""),
		ID:     none,
		Event:  jsonField("event"),
		Signed: []string{"Honeybadger-Token"},
	},
	"standard-webhooks": {
		Verify: standardVerify,
		ID:     header("Webhook-Id"),
		Event:  jsonField("type"),
		Secret: standardSecret,
		Signed: []string{"Webhook-Signature"},
	},
	"token": {
		Verify: token("Authorization", "Bearer "),
		ID:     none,
		Event:  none,
		Signed: []string{"Authorization"},
	},
}

func none(Request) string { return "" }

func header(name string) func(Request) string {
	return func(r Request) string { return r.Header.Get(name) }
}

// jsonField reads a string at a path in a JSON body, "" when the body is not JSON or the path is not a string.
func jsonField(path ...string) func(Request) string {
	return func(r Request) string {
		var node any

		err := json.Unmarshal(r.Body, &node)
		if err != nil {
			return ""
		}

		for _, key := range path {
			object, _ := node.(map[string]any)
			node = object[key]
		}

		value, _ := node.(string)

		return value
	}
}

// hmacBody is the scheme most senders use: a hex HMAC-SHA256 of the raw body behind a prefix in one header — Scheme at its defaults.
func hmacBody(name, prefix string) func(Request, []byte, time.Time) bool {
	return Scheme{Header: name, Prefix: prefix}.Verify
}

// token is a sender that presents the secret itself rather than a signature.
func token(name, prefix string) func(Request, []byte, time.Time) bool {
	return func(r Request, secret []byte, _ time.Time) bool {
		got, found := strings.CutPrefix(r.Header.Get(name), prefix)

		return found && hmac.Equal([]byte(got), secret)
	}
}

func sign(secret []byte, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, secret)
	for _, part := range parts {
		mac.Write(part)
	}

	return mac.Sum(nil)
}

func equalHex(got string, want []byte) bool {
	return hmac.Equal([]byte(got), []byte(hex.EncodeToString(want)))
}

// fresh reports whether a signed unix timestamp is within Tolerance of now.
func fresh(stamp string, now time.Time) bool {
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return false
	}

	skew := now.Sub(time.Unix(seconds, 0))

	return skew <= Tolerance && skew >= -Tolerance
}

// slackVerify is v0=hex(HMAC("v0:" + timestamp + ":" + body)).
func slackVerify(r Request, secret []byte, now time.Time) bool {
	stamp := r.Header.Get("X-Slack-Request-Timestamp")
	got, found := strings.CutPrefix(r.Header.Get("X-Slack-Signature"), "v0=")

	return found && fresh(stamp, now) && equalHex(got, sign(secret, []byte("v0:"+stamp+":"), r.Body))
}

// slackEvent is the Events API's inner type for an event_callback, and the envelope's type otherwise; a form-encoded body (a slash command) has neither.
func slackEvent(r Request) string {
	if kind := jsonField("type")(r); kind != "event_callback" {
		return kind
	}

	return jsonField("event", "type")(r)
}

// slackHandshake answers the url_verification Slack sends, signed, when an Events API URL is saved.
func slackHandshake(r Request) ([]byte, bool) {
	if jsonField("type")(r) != "url_verification" {
		return nil, false
	}

	return []byte(jsonField("challenge")(r)), true
}

// stripeVerify is t=<unix>,v1=<hex>[,v1=<hex>] over timestamp + "." + body; more than one v1 during a secret roll.
func stripeVerify(r Request, secret []byte, now time.Time) bool {
	var (
		stamp      string
		signatures []string
	)

	for part := range strings.SplitSeq(r.Header.Get("Stripe-Signature"), ",") {
		key, value, _ := strings.Cut(part, "=")

		switch key {
		case "t":
			stamp = value
		case "v1":
			signatures = append(signatures, value)
		}
	}

	if !fresh(stamp, now) {
		return false
	}

	want := sign(secret, []byte(stamp+"."), r.Body)

	for _, got := range signatures {
		if equalHex(got, want) {
			return true
		}
	}

	return false
}

// pagerdutyVerify is v1=<hex>[,v1=<hex>], one per secret while PagerDuty rotates.
func pagerdutyVerify(r Request, secret []byte, _ time.Time) bool {
	want := sign(secret, r.Body)

	for part := range strings.SplitSeq(r.Header.Get("X-PagerDuty-Signature"), ",") {
		got, found := strings.CutPrefix(strings.TrimSpace(part), "v1=")
		if found && equalHex(got, want) {
			return true
		}
	}

	return false
}

// standardVerify is https://www.standardwebhooks.com: space-separated v1,<base64> signatures over id + "." + timestamp + "." + body.
func standardVerify(r Request, secret []byte, now time.Time) bool {
	id := r.Header.Get("Webhook-Id")
	stamp := r.Header.Get("Webhook-Timestamp")

	if id == "" || !fresh(stamp, now) {
		return false
	}

	want := base64.StdEncoding.EncodeToString(sign(secret, []byte(id+"."+stamp+"."), r.Body))

	for _, candidate := range strings.Fields(r.Header.Get("Webhook-Signature")) {
		got, found := strings.CutPrefix(candidate, "v1,")
		if found && hmac.Equal([]byte(got), []byte(want)) {
			return true
		}
	}

	return false
}

// standardSecret decodes the whsec_-prefixed base64 secret a Standard Webhooks sender hands out; the key is the decoded bytes, not the string.
func standardSecret(raw string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "whsec_")) //nolint:wrapcheck // Verify reports it as unauthorized
}
