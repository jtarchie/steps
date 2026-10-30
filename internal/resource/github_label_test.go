package resource

import (
	"context"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
)

// TestGitHubLabelOutVersionAndRequests: the version records what was asked, keyed as github-reaction's is, with a side left out when the put did not ask for it.
func TestGitHubLabelOutVersionAndRequests(t *testing.T) {
	for name, tc := range map[string]struct {
		params   map[string]any
		requests []string
		version  map[string]any
	}{
		"swap": {
			params:   map[string]any{"add": []any{"factory:building", "p1"}, "remove": []any{"factory:queued", "needs/review"}},
			requests: []string{`POST /repos/acme/app/issues/12/labels {"labels":["factory:building","p1"]}`, "DELETE /repos/acme/app/issues/12/labels/factory:queued ", "DELETE /repos/acme/app/issues/12/labels/needs%2Freview "},
			version:  map[string]any{"number": "12", "add": "factory:building, p1", "remove": "factory:queued, needs/review"},
		},
		"add only, by number": {
			params:   map[string]any{"add": []any{"factory:building"}, "number": "30"},
			requests: []string{`POST /repos/acme/app/issues/30/labels {"labels":["factory:building"]}`},
			version:  map[string]any{"number": "30", "add": "factory:building"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var (
				mu   sync.Mutex
				seen []string
			)

			connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)

				mu.Lock()
				seen = append(seen, r.Method+" "+r.URL.EscapedPath()+" "+string(body))
				mu.Unlock()

				_, _ = w.Write([]byte(`[]`))
			})

			version, err := githubLabelOut(context.Background(),
				map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint},
				tc.params,
				PutInputs{Names: []string{"issue"}, Versions: map[string]map[string]any{"issue": {"number": "12"}}},
				t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()

			if !slices.Equal(seen, tc.requests) {
				t.Errorf("requests = %q, want %q", seen, tc.requests)
			}

			if !maps.Equal(version, tc.version) {
				t.Errorf("version = %v, want %v", version, tc.version)
			}
		})
	}
}
