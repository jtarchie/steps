package web

import (
	"net/http"
	"strings"
	"testing"
)

// The tab narrows like every list: by what needs a person, by transport and auth, and by what was typed — name, address or user.
func TestTheMCPTabNarrows(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})

	row := func(name string) string { return `id="mcp-` + name + `"` }

	for _, tc := range []struct {
		query        string
		want, refuse []string
	}{
		{"?transport=stdio", []string{row("gopls"), row("perstep")}, []string{row("linear"), row("github")}},
		{"?auth=oauth", []string{row("linear")}, []string{row("github"), row("gopls")}},
		{"?status=attention", []string{row("linear"), row("github")}, []string{row("perstep")}},
		{"?q=githubcopilot", []string{row("github")}, []string{row("linear")}},
		{"?q=TRIAGER", []string{row("linear")}, []string{row("github")}},
		{"?q=nothing-is-called-this", []string{"Nothing matches."}, []string{row("linear")}},
	} {
		code, body := get(t, server, "/p/demo/mcp"+tc.query)
		if code != http.StatusOK {
			t.Fatalf("GET /p/demo/mcp%s = %d", tc.query, code)
		}

		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("mcp%s is missing %s", tc.query, want)
			}
		}

		for _, refuse := range tc.refuse {
			if strings.Contains(body, refuse) {
				t.Errorf("mcp%s still shows %s", tc.query, refuse)
			}
		}
	}
}
