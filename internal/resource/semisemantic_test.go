package resource

import "testing"

// TestSemiVersionOrderIsUpstreams: the cases go-semi-semantic's own suite pins, plus the one that decides ruby/ruby under Concourse's default filter: an underscored tag is one word, which sorts after every number, and two words compare as text.
func TestSemiVersionOrderIsUpstreams(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2", "10", -1},
		{"1.0", "1.0.0", 0},
		{"1.0.1", "1.0", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
		{"1.0.0-rc.10", "1.0.0-rc.9", 1},
		{"1.0.0", "1.0.0+build.1", -1},
		{"1.0.0+build.2", "1.0.0+build.1", 1},
		{"1.0.0-alpha", "1.0.0-1", 1},
		{"3_4_13", "4.0.10", 1},
		{"3_4_9", "3_4_13", 1},
		{"1.01", "1.2", 1},
	} {
		a, err := parseSemiVersion(tc.a)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.a, err)
		}

		b, err := parseSemiVersion(tc.b)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.b, err)
		}

		if got := a.Compare(b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}

		if got := b.Compare(a); got != -tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}

	for _, bad := range []string{"", "1..2", "x y", "1.0-", "-rc.1", "99999999999999999999"} {
		_, err := parseSemiVersion(bad)
		if err == nil {
			t.Errorf("parse %q succeeded, want it refused", bad)
		}
	}
}
