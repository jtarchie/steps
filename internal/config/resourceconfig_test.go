package config

import (
	"fmt"
	"testing"
)

// TestResourceConfigHashesFollowTypeAndSource: what empties a resource's version history (steps#280) is what Concourse keys a resource_config on — the type, a custom type's own image, and the source — and nothing that only says where or how long a check runs, nor a type's commands, which a steps upgrade may change under a built-in.
func TestResourceConfigHashesFollowTypeAndSource(t *testing.T) {
	t.Parallel()

	pipeline := func(typeImage, check, resourceType, source, extra string) string {
		return fmt.Sprintf(`
resource_types:
- name: feed
  image: %s
  config:
    check: %s
    in: "true"
- name: other
  image: alpine:3.20
  config:
    check: echo '[]'
    in: "true"
resources:
- name: items
  type: %s
  source: %s
%s
jobs:
- name: build
  plan:
  - get: items
`, typeImage, check, resourceType, source, extra)
	}

	hash := func(yaml string) string {
		t.Helper()

		hashes, err := loadOK(t, yaml).ResourceConfigHashes()
		if err != nil {
			t.Fatal(err)
		}

		return hashes["items"]
	}

	base := hash(pipeline("alpine:3.20", "echo '[]'", "feed", "{at: a}", ""))

	for _, tc := range []struct {
		name    string
		yaml    string
		changed bool
	}{
		{"the same pipeline", pipeline("alpine:3.20", "echo '[]'", "feed", "{at: a}", ""), false},
		{"its source", pipeline("alpine:3.20", "echo '[]'", "feed", "{at: b}", ""), true},
		{"its type", pipeline("alpine:3.20", "echo '[]'", "other", "{at: a}", ""), true},
		{"its type's image", pipeline("alpine:3.21", "echo '[]'", "feed", "{at: a}", ""), true},
		{"its type's commands", pipeline("alpine:3.20", "echo '[ ]'", "feed", "{at: a}", ""), false},
		{"where and how long its check runs", pipeline("alpine:3.20", "echo '[]'", "feed", "{at: a}", "  check_timeout: 5m\n  tags: [edge]"), false},
	} {
		if got := hash(tc.yaml) != base; got != tc.changed {
			t.Errorf("changing %s: history reset = %v, want %v", tc.name, got, tc.changed)
		}
	}
}
