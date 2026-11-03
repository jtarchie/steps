package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/redact"
)

// TestMCPToolImplMasksStructuredContentBeforeSpill pins that structured content is scrubbed like text content is: the spill file on disk and the inline return must not hold a planted secret, whether the payload spills or fits inline.
func TestMCPToolImplMasksStructuredContentBeforeSpill(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-token-value"

	secrets := &redact.Secrets{}
	secrets.Add(secret)

	ctx := redact.WithSecrets(context.Background(), secrets)

	for name, blob := range map[string]string{
		"spilled": strings.Repeat("x", maxToolOutputBytes) + secret,
		"inline":  "prefix " + secret,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := &fakeMCPClient{result: &sdkmcp.CallToolResult{
				StructuredContent: map[string]any{"blob": blob},
			}}
			dir := t.TempDir()

			impl := mcpToolImpl(client, "search_issues", maxToolOutputBytes, nil)
			result := impl(ctx, map[string]any{}, toolEnv{spillDir: dir})

			returned, err := json.Marshal(result["structured_content"])
			if err != nil {
				t.Fatal(err)
			}

			if strings.Contains(string(returned), secret) {
				t.Errorf("returned structured_content holds the secret: %.120s", returned)
			}

			files, err := filepath.Glob(filepath.Join(dir, "*"))
			if err != nil {
				t.Fatal(err)
			}

			if name == "spilled" && len(files) == 0 {
				t.Fatal("expected a spill file")
			}

			for _, f := range files {
				b, err := os.ReadFile(f) //nolint:gosec // path globbed from this test's own TempDir
				if err != nil {
					t.Fatal(err)
				}

				if strings.Contains(string(b), secret) {
					t.Errorf("spill file %s holds the secret", f)
				}
			}
		})
	}
}
