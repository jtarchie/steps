package cli

// What a page render actually pays to ask what a saved token is worth. The
// nav badge asks this for every oauth server of every SERVED pipeline on
// every request, including each open tab's 2.5s self-poll — which is a very
// different access pattern from the mcp tab it was written for.

import (
	"path/filepath"
	"testing"
	"time"

	stepsmcp "github.com/jtarchie/steps/internal/mcp"
)

func BenchmarkMCPState(b *testing.B) {
	dir := b.TempDir()
	b.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	b.Setenv("HOME", dir)

	held := servingDaemon(b)
	setPipeline(b, held, "app", mcpStatePipeline)

	target := held.server.Lookup("app")

	for _, test := range []struct {
		name  string
		token *stepsmcp.TokenFile
	}{
		{name: "nobody has logged in"},
		{name: "connected", token: &stepsmcp.TokenFile{
			Endpoint: "https://tracker.example/mcp", AccessToken: secretToken,
			RefreshToken: "r", Expiry: time.Now().Add(time.Hour),
		}},
	} {
		writeTokenFile(b, test.token)

		b.Run(test.name, func(b *testing.B) {
			for b.Loop() {
				_ = held.MCPState(target, "tracker")
			}
		})
	}
}
