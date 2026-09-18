package webhook

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // a sender's choice of HMAC-SHA1 is still a keyed MAC, and some senders offer nothing else
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"maps"
	"slices"
	"strings"
	"time"
)

// Custom is the provider whose scheme is the resource's own source.signature.
const Custom = "custom"

// Scheme is an HMAC carried in one header. It is what provider: custom compiles into and what hmacBody is at its defaults (hex, sha256, the raw body signed), so a described scheme and a built-in one are checked by the same code.
type Scheme struct {
	Header    string
	Prefix    string
	Encoding  string
	Algorithm string
	// Signed builds the message the sender signed; nil means the raw body.
	Signed func(Input) (string, error)
	// Timestamp, when set, is the unix time the sender signed, refused outside Tolerance. It only protects anything when Signed includes it.
	Timestamp func(Input) (string, error)
}

// Verify is the Provider.Verify for this scheme. An expression that fails to evaluate verifies nothing.
func (s Scheme) Verify(r Request, secret []byte, now time.Time) bool {
	got, found := strings.CutPrefix(r.Header.Get(s.Header), s.Prefix)
	if !found {
		return false
	}

	in := r.input(Custom, "")
	message := r.Body

	if s.Signed != nil {
		signed, err := s.Signed(in)
		if err != nil {
			return false
		}

		message = []byte(signed)
	}

	if s.Timestamp != nil {
		stamp, err := s.Timestamp(in)
		if err != nil || !fresh(stamp, now) {
			return false
		}
	}

	mac := hmac.New(s.hash(), secret)
	mac.Write(message)

	return hmac.Equal([]byte(got), []byte(s.encode(mac.Sum(nil))))
}

func (s Scheme) hash() func() hash.Hash {
	switch s.Algorithm {
	case "sha1":
		return sha1.New
	case "sha512":
		return sha512.New
	default:
		return sha256.New
	}
}

func (s Scheme) encode(sum []byte) string {
	if s.Encoding == "base64" {
		return base64.StdEncoding.EncodeToString(sum)
	}

	return hex.EncodeToString(sum)
}

// ProviderNames is every value source.provider may take, sorted: the table, and custom.
func ProviderNames() []string {
	return slices.Sorted(slices.Values(append(slices.Collect(maps.Keys(Providers)), Custom)))
}
