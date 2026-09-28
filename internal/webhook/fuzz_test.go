package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

// fuzzSigner is this test's own reading of one provider's scheme: the credential header it sets, and whether a candidate value for it is the correct one. Written apart from providers.go so a shared misreading has to be made twice.
type fuzzSigner struct {
	header string
	// value is the whole header for a correctly signed delivery.
	value func(key, body []byte, stamp, id string) string
	// wrap places a fuzzed candidate where the signature goes; ok false skips a candidate whose separators would make it several candidates.
	wrap func(candidate, stamp string) (string, bool)
	// secret turns the key into what the operator configures.
	secret     func(key []byte) string
	stamped    bool
	bodySigned bool
}

func fuzzHexMAC(key []byte, parts ...string) string {
	mac := hmac.New(sha256.New, key)
	for _, part := range parts {
		mac.Write([]byte(part))
	}

	return hex.EncodeToString(mac.Sum(nil))
}

func fuzzPrefixed(header, prefix string) fuzzSigner {
	return fuzzSigner{
		header:     header,
		value:      func(key, body []byte, _, _ string) string { return prefix + fuzzHexMAC(key, string(body)) },
		wrap:       func(candidate, _ string) (string, bool) { return prefix + candidate, true },
		secret:     func(key []byte) string { return string(key) },
		bodySigned: true,
	}
}

func fuzzToken(header, prefix string) fuzzSigner {
	return fuzzSigner{
		header: header,
		value:  func(key, _ []byte, _, _ string) string { return prefix + string(key) },
		wrap:   func(candidate, _ string) (string, bool) { return prefix + candidate, true },
		secret: func(key []byte) string { return string(key) },
	}
}

var fuzzSigners = map[string]fuzzSigner{
	"github":      fuzzPrefixed("X-Hub-Signature-256", "sha256="),
	"gitea":       fuzzPrefixed("X-Gitea-Signature", ""),
	"forgejo":     fuzzPrefixed("X-Forgejo-Signature", ""),
	"bitbucket":   fuzzPrefixed("X-Hub-Signature", "sha256="),
	"sentry":      fuzzPrefixed("Sentry-Hook-Signature", ""),
	"linear":      fuzzPrefixed("Linear-Signature", ""),
	"gitlab":      fuzzToken("X-Gitlab-Token", ""),
	"honeybadger": fuzzToken("Honeybadger-Token", ""),
	"token":       fuzzToken("Authorization", "Bearer "),
	"slack": {
		header: "X-Slack-Signature",
		value: func(key, body []byte, stamp, _ string) string {
			return "v0=" + fuzzHexMAC(key, "v0:", stamp, ":", string(body))
		},
		wrap:       func(candidate, _ string) (string, bool) { return "v0=" + candidate, true },
		secret:     func(key []byte) string { return string(key) },
		stamped:    true,
		bodySigned: true,
	},
	"stripe": {
		header: "Stripe-Signature",
		value: func(key, body []byte, stamp, _ string) string {
			return "t=" + stamp + ",v1=" + fuzzHexMAC(key, stamp, ".", string(body))
		},
		wrap: func(candidate, stamp string) (string, bool) {
			return "t=" + stamp + ",v1=" + candidate, !strings.Contains(candidate, ",")
		},
		secret:     func(key []byte) string { return string(key) },
		stamped:    true,
		bodySigned: true,
	},
	"pagerduty": {
		header: "X-PagerDuty-Signature",
		value:  func(key, body []byte, _, _ string) string { return "v1=" + fuzzHexMAC(key, string(body)) },
		wrap: func(candidate, _ string) (string, bool) {
			return "v1=" + candidate, !strings.Contains(candidate, ",") && strings.TrimSpace(candidate) == candidate
		},
		secret:     func(key []byte) string { return string(key) },
		bodySigned: true,
	},
	"standard-webhooks": {
		header: "Webhook-Signature",
		value: func(key, body []byte, stamp, id string) string {
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(id + "." + stamp + "." + string(body)))

			return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		},
		wrap: func(candidate, _ string) (string, bool) {
			return "v1," + candidate, !strings.ContainsFunc(candidate, unicode.IsSpace)
		},
		secret:     func(key []byte) string { return "whsec_" + base64.StdEncoding.EncodeToString(key) },
		stamped:    true,
		bodySigned: true,
	},
}

func fuzzRequest(signer fuzzSigner, credential string, body []byte, stamp, id string) Request {
	header := http.Header{}
	header.Set(signer.header, credential)
	header.Set("X-Slack-Request-Timestamp", stamp)
	header.Set("Webhook-Timestamp", stamp)
	header.Set("Webhook-Id", id)

	return Request{Method: http.MethodPost, Header: header, Body: body}
}

// FuzzVerify holds every provider to a signature computed here: the correct credential verifies, and nothing else does — not a different candidate, not a different body, not a stale timestamp, not an empty secret.
//
//nolint:cyclop // one branch per property the fuzzed input is held to
func FuzzVerify(f *testing.F) {
	for name := range Providers {
		if _, ok := fuzzSigners[name]; !ok {
			f.Fatalf("provider %q has no signer in fuzzSigners, so FuzzVerify never checks it", name)
		}
	}

	f.Add(uint8(0), []byte("secret"), []byte(`{"a":1}`), []byte(`{"a":2}`), "deadbeef", int64(0))
	f.Add(uint8(5), []byte("k"), []byte(""), []byte("x"), "", int64(301))
	f.Add(uint8(9), []byte{0, 1, 2}, []byte("body"), []byte("body"), "v1=", int64(-300))
	f.Add(uint8(11), []byte("k"), []byte("b"), []byte("b"), "", int64(301))
	f.Add(uint8(10), []byte("k"), []byte("b"), []byte("b"), "", int64(-301))

	names := make([]string, 0, len(fuzzSigners))
	for name := range Providers {
		names = append(names, name)
	}

	slices.Sort(names)

	now := time.Unix(1_700_000_000, 0)

	f.Fuzz(func(t *testing.T, pick uint8, key, body, otherBody []byte, candidate string, skew int64) {
		name := names[int(pick)%len(names)]
		signer := fuzzSigners[name]
		receiver := &Receiver{Provider: name}

		if len(key) == 0 {
			// An empty secret refuses everything, and the rest of the properties need a key to sign with.
			req := fuzzRequest(signer, signer.value(key, body, "1700000000", "id"), body, "1700000000", "id")
			if receiver.Verify(req, "", now) == nil {
				t.Fatalf("%s verified with an empty secret", name)
			}

			return
		}

		if !signer.stamped {
			skew = 0
		}

		if skew > math.MaxInt32 || skew < math.MinInt32 {
			skew %= math.MaxInt32
		}

		stamp := strconv.FormatInt(now.Unix()+skew, 10)
		id := "msg_1"
		secret := signer.secret(key)
		inside := skew >= -int64(Tolerance/time.Second) && skew <= int64(Tolerance/time.Second)
		correct := signer.value(key, body, stamp, id)

		if got := receiver.Verify(fuzzRequest(signer, correct, body, stamp, id), secret, now) == nil; got != inside {
			t.Fatalf("%s: correct credential, skew %ds: verified=%v, want %v", name, skew, got, inside)
		}

		if !inside {
			return
		}

		if signer.bodySigned {
			got := receiver.Verify(fuzzRequest(signer, correct, otherBody, stamp, id), secret, now) == nil
			if want := bytes.Equal(body, otherBody); got != want {
				t.Fatalf("%s: signature over %q, delivered %q: verified=%v, want %v", name, body, otherBody, got, want)
			}
		}

		wrapped, ok := signer.wrap(candidate, stamp)
		if !ok {
			return
		}

		got := receiver.Verify(fuzzRequest(signer, wrapped, body, stamp, id), secret, now) == nil
		if want := wrapped == correct; got != want {
			t.Fatalf("%s: credential %q (correct %q): verified=%v, want %v", name, wrapped, correct, got, want)
		}
	})
}

// FuzzFresh checks the replay window against exact integer arithmetic, including the timestamps near the ends of int64 where time.Unix and Sub saturate or wrap.
func FuzzFresh(f *testing.F) {
	f.Add("1700000000", int64(1_700_000_000))
	f.Add("9223372036854775807", int64(1_700_000_000))
	f.Add("-9223372036854775808", int64(0))
	f.Add("+1700000300", int64(1_700_000_000))
	f.Add("1700000301", int64(1_700_000_000))
	f.Add("1699999699", int64(1_700_000_000))

	f.Fuzz(func(t *testing.T, stamp string, nowUnix int64) {
		// Callers pass time.Now; keep now within a few thousand years of it.
		nowUnix %= 1 << 40

		got := fresh(stamp, time.Unix(nowUnix, 0))

		seconds, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			if got {
				t.Fatalf("fresh(%q) accepted a stamp that is not an int64", stamp)
			}

			return
		}

		skew := new(big.Int).Sub(big.NewInt(nowUnix), big.NewInt(seconds))
		limit := big.NewInt(int64(Tolerance / time.Second))
		want := skew.CmpAbs(limit) <= 0

		if got != want {
			t.Fatalf("fresh(%q, %d) = %v, want %v (skew %s s)", stamp, nowUnix, got, want, skew)
		}
	})
}

// FuzzParseRequest: the body is every byte after the first blank line, and parsing the head never writes over it.
func FuzzParseRequest(f *testing.F) {
	f.Add([]byte("POST /hooks/push?x=1 HTTP/1.1\nX-GitHub-Event: push\n\n{\"after\":\"aaa\"}\n"))
	f.Add([]byte("POST / HTTP/1.1\r\nHost: x\r\n\r\nbody\n\nmore"))
	f.Add([]byte("GET / HTTP/1.1\n\n"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		original := bytes.Clone(raw)

		req, err := ParseRequest(raw)

		if !bytes.Equal(raw, original) {
			t.Fatalf("ParseRequest modified its input: %q became %q", original, raw)
		}

		if err != nil {
			return
		}

		want := ""

		if _, body, found := bytes.Cut(original, []byte("\r\n\r\n")); found {
			want = string(body)
		} else if _, body, found := bytes.Cut(original, []byte("\n\n")); found {
			want = string(body)
		} else {
			t.Fatalf("parsed %q, which has no blank line", original)
		}

		if string(req.Body) != want {
			t.Fatalf("body = %q, want %q", req.Body, want)
		}
	})
}

// FuzzAccept runs a verified delivery with no pipeline expressions through every provider: it always yields a version with an id or a handshake, keeps the body byte for byte, and never records a credential header.
//
//nolint:cyclop // one branch per property the fuzzed input is held to
func FuzzAccept(f *testing.F) {
	f.Add(uint8(0), "X-Hub-Signature-256: sha256=abc\nAuthorization: Bearer s\nCookie: c\nX-GitHub-Event: push", []byte(`{"id":"x"}`))
	f.Add(uint8(3), "X-Forgejo-Signature: a\nx-gitea-signature: b\nX-Gogs-Signature: c", []byte(`{"type":"url_verification","challenge":"c"}`))
	f.Add(uint8(7), "authorization: x", []byte(`{"event":{"id":1}}`))

	names := make([]string, 0, len(Providers))
	for name := range Providers {
		names = append(names, name)
	}

	slices.Sort(names)

	f.Fuzz(func(t *testing.T, pick uint8, headers string, body []byte) {
		name := names[int(pick)%len(names)]

		header := http.Header{}

		for line := range strings.SplitSeq(headers, "\n") {
			key, value, _ := strings.Cut(line, ":")
			header.Add(key, strings.TrimSpace(value))
		}

		result, err := (&Receiver{Provider: name}).Accept(Request{Method: http.MethodPost, Header: header, Body: body})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if result.Filtered {
			t.Fatalf("%s: filtered with no filter", name)
		}

		if result.Handshake != nil {
			if name != "slack" {
				t.Fatalf("%s answered a handshake", name)
			}

			return
		}

		if id, _ := result.Delivery.Version["id"].(string); id == "" {
			t.Fatalf("%s: version %v has no id", name, result.Delivery.Version)
		}

		if !bytes.Equal(result.Delivery.Body, body) {
			t.Fatalf("%s: body %q recorded as %q", name, body, result.Delivery.Body)
		}

		secret := append([]string{"Authorization", "Cookie"}, Providers[name].Signed...)
		for kept := range result.Delivery.Headers {
			for _, credential := range secret {
				if strings.EqualFold(kept, credential) {
					t.Fatalf("%s: recorded the credential header %q", name, kept)
				}
			}
		}
	})
}
