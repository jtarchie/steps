package resource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinearIssueOutRefusesAnUnsuccessfulCreate: issueCreate can answer 200 with success false and no issue, and that is a red put, never a version naming nothing.
func TestLinearIssueOutRefusesAnUnsuccessfulCreate(t *testing.T) {
	for name, issue := range map[string]string{
		"success false":      `{"issueCreate":{"success":false,"issue":{"id":"u1","identifier":"ENG-1","url":"","title":""}}}`,
		"no identifier back": `{"issueCreate":{"success":true,"issue":{"id":"u1","identifier":"","url":"","title":""}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Query string `json:"query"`
				}

				_ = json.NewDecoder(r.Body).Decode(&body)

				if strings.Contains(body.Query, "issueCreate") {
					_, _ = w.Write([]byte(`{"data":` + issue + `}`))

					return
				}

				_, _ = w.Write([]byte(`{"data":{"teams":{"nodes":[{"id":"tm","key":"ENG","name":"Engineering","states":{"nodes":[]}}]}}}`))
			}))
			t.Cleanup(server.Close)

			t.Setenv("RESOURCE_TEST_LINEAR_KEY", "lin_api_k")

			dir := t.TempDir()
			for file, text := range map[string]string{"title": "A title\n", "body.md": "A body.\n"} {
				err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o600)
				if err != nil {
					t.Fatal(err)
				}
			}

			version, err := linearIssueOut(context.Background(),
				map[string]any{"team": "ENG", "token_env": "RESOURCE_TEST_LINEAR_KEY", "endpoint": server.URL},
				map[string]any{"title_file": "title", "body_file": "body.md"}, PutInputs{}, dir)
			if err == nil || !strings.Contains(err.Error(), "issueCreate in team ENG did not succeed") {
				t.Fatalf("linearIssueOut = %v, %v; want it refused", version, err)
			}
		})
	}
}
