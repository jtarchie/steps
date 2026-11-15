package dockerapi

import "testing"

// A registry digest is the same on every daemon that pulled the image; the image id is not, so it is only the answer for an image built where it runs.
func TestPickDigestPrefersTheRegistrys(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		image       string
		repoDigests []string
		id, want    string
	}{
		"pulled":        {"golang:1.27", []string{"golang@sha256:" + hexA}, "sha256:id", "sha256:" + hexA},
		"built locally": {"mine:dev", nil, "sha256:id", "sha256:id"},
		"a name with no digest in it is passed over": {"golang:1.27", []string{"golang", "golang@sha256:" + hexB}, "sha256:id", "sha256:" + hexB},
		"nothing known": {"golang:1.27", nil, "", ""},
		// One image id, two repositories: the digest must be one the named repository serves.
		"the named repository wins":               {"ghcr.io/me/golang:1.27", []string{"golang@sha256:" + hexA, "ghcr.io/me/golang@sha256:" + hexB}, "sha256:id", "sha256:" + hexB},
		"another repository's beats the image id": {"mine:dev", []string{"golang@sha256:" + hexA}, "sha256:id", "sha256:" + hexA},
	} {
		if got := pickDigest(tc.image, tc.repoDigests, tc.id); got != tc.want {
			t.Errorf("%s: pickDigest = %q, want %q", name, got, tc.want)
		}
	}
}

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)
