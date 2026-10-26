package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEveryAppIsGivenTheAddress: every tab is an errand that ends at the same address, and one that never shows it is one nobody can finish.
func TestEveryAppIsGivenTheAddress(t *testing.T) {
	t.Parallel()

	const address = "https://steps.example/mcp/x/ops/support"

	seen := map[string]bool{}

	for _, group := range connectGroups("support", address) {
		for _, client := range group.Clients {
			if seen[client.Slug] {
				t.Errorf("two tabs share the slug %q, so one label would open the other's panel", client.Slug)
			}

			seen[client.Slug] = true

			if !showsAddress(t, client, address) {
				t.Errorf("%s never shows the address", client.Slug)
			}
		}
	}
}

func showsAddress(t *testing.T, client connectClient, address string) bool {
	t.Helper()

	var mentions bool

	for _, step := range client.Steps {
		if strings.Contains(step.Snippet, address) || strings.Contains(string(step.Link), address) {
			mentions = true
		}

		if strings.HasPrefix(step.Snippet, "{") && !json.Valid([]byte(step.Snippet)) {
			t.Errorf("%s: %q is not valid JSON", client.Slug, step.Title)
		}
	}

	return mentions
}

// TestAnAddressThatDoesNotParseIsNotOffered: --external-url is validated before the daemon starts, so this is a fallback; it fails closed, since an address nobody can read is not one a vendor can reach.
func TestAnAddressThatDoesNotParseIsNotOffered(t *testing.T) {
	t.Parallel()

	clients := []connectClient{{Name: "Claude", Vendor: "Anthropic"}}
	if fix := judgeReach(clients, "http://[::1"); fix == "" || clients[0].Blocked == "" {
		t.Errorf("an unparseable address offered Claude: fix=%q blocked=%q", fix, clients[0].Blocked)
	}
}
