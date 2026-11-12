package resource

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func prSource(connection config.GitHubConnection) map[string]any {
	return map[string]any{"repo": connection.Repo, "token_env": connection.TokenEnv, "endpoint": connection.Endpoint}
}

// TestGitHubPRRefusesSeveralOpenForAHead: GitHub allows an open pull request per head and base, so two for one head is ambiguity the put names rather than resolves; and one whose head is another branch, which GitHub's filter should never answer, is not the branch's.
func TestGitHubPRRefusesSeveralOpenForAHead(t *testing.T) {
	for name, tc := range map[string]struct {
		listed string
		want   string
	}{
		"two":           {`[{"number":3,"head":{"ref":"feat"}},{"number":4,"head":{"ref":"feat"}}]`, "#3, #4"},
		"another ref":   {`[{"number":3,"head":{"ref":"other"}}]`, "title_file is required"},
		"none":          {`[]`, "title_file is required"},
		"one, and base": {`[{"number":3,"head":{"ref":"feat"},"base":{"ref":"develop"}}]`, "params.base"},
	} {
		var queries []string

		connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			_, _ = w.Write([]byte(tc.listed))
		})

		params := map[string]any{"head": "feat"}
		if name == "one, and base" {
			params["base"] = "main"
		}

		_, err := githubPROut(context.Background(), prSource(connection), params, PutInputs{}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}

		if len(queries) != 1 || queries[0] != "state=open&per_page=100&head=acme%3Afeat" {
			t.Errorf("%s: lookups %q, want the one open-state lookup for acme:feat", name, queries)
		}
	}
}

// TestGitHubPRReadyFailureIsNamed: GraphQL answers 200 with errors, which a put must not mistake for a flipped draft.
func TestGitHubPRReadyFailureIsNamed(t *testing.T) {
	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"errors":[{"message":"Resource not accessible by personal access token"}]}`))

			return
		}

		_, _ = w.Write([]byte(`[{"number":3,"node_id":"PR_x","draft":true,"head":{"ref":"feat","sha":"abc"},"base":{"ref":"main"}}]`))
	})

	_, err := githubPROut(context.Background(), prSource(connection), map[string]any{"head": "feat", "ready": true}, PutInputs{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "#3 ready") || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Errorf("err = %v, want the failure naming #3 and GitHub's words", err)
	}
}

// TestGitHubPRUnreadableFileFailsBeforeAnyRequest: a title file that cannot be read is found before the lookup, so nothing is sent for a put that could not finish.
func TestGitHubPRUnreadableFileFailsBeforeAnyRequest(t *testing.T) {
	requests := 0
	connection := githubServer(t, func(http.ResponseWriter, *http.Request) { requests++ })

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "title"), []byte("one\ntwo\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = githubPROut(context.Background(), prSource(connection), map[string]any{"head": "feat", "title_file": "title"}, PutInputs{}, dir)
	if err == nil || !strings.Contains(err.Error(), "a title is one line") || requests != 0 {
		t.Errorf("err = %v after %d requests, want the two-line title refused with none sent", err, requests)
	}
}
