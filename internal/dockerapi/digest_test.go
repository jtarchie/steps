package dockerapi

import "testing"

// A registry digest is the same on every daemon that pulled the image; the image id is not, so it is only the answer for an image built where it runs.
func TestPickDigestPrefersTheRegistrys(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		repoDigests []string
		id, want    string
	}{
		"pulled":        {[]string{"golang@sha256:aaa"}, "sha256:id", "sha256:aaa"},
		"built locally": {nil, "sha256:id", "sha256:id"},
		"a name with no digest in it is passed over": {[]string{"golang", "golang@sha256:bbb"}, "sha256:id", "sha256:bbb"},
		"nothing known": {nil, "", ""},
	} {
		if got := pickDigest(tc.repoDigests, tc.id); got != tc.want {
			t.Errorf("%s: pickDigest = %q, want %q", name, got, tc.want)
		}
	}
}
