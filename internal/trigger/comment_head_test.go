package trigger

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// commentedPR serves pull request #42's head and the /steps comments on it, newest first.
func commentedPR(t *testing.T, head *atomic.Value, comments *atomic.Value) *config.Config {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/app/issues/comments":
			ids, _ := comments.Load().([]int)
			listed := make([]string, 0, len(ids))

			for _, id := range ids {
				listed = append(listed, fmt.Sprintf(`{"id":%d,"body":"/steps go","updated_at":"2026-11-01T00:00:0%dZ","user":{"login":"alice"},"html_url":"https://x/acme/app/pull/42#c","issue_url":"https://x/repos/acme/app/issues/42"}`, id, id))
			}

			_, _ = w.Write([]byte("[" + strings.Join(listed, ",") + "]"))
		case "/repos/acme/app/pulls/42":
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q}}`, head.Load())
		default:
			t.Errorf("unserved: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	t.Setenv("TRIGGER_TEST_GH_TOKEN", "t0ken")

	return loadConfig(t, t.TempDir(), `
defaults:
  preflight:
    disabled: true
resources:
- name: command
  type: github-comments
  source:
    repo: acme/app
    endpoint: `+server.URL+`
    token_env: TRIGGER_TEST_GH_TOKEN
    kinds: [conversation]
jobs:
- name: act
  plan:
  - get: command
    trigger: true
    version: every
`)
}

func filedCommands(t *testing.T, st store.Versions) string {
	t.Helper()

	filed, err := st.ResourceVersionsJSON(t.Context(), "command")
	if err != nil {
		t.Fatal(err)
	}

	return fmt.Sprint(filed)
}

// TestCommentKeepsTheHeadItWasFirstSeenAt crosses the seam the binding rests on: the check learns what it already reported from version history, so a head that moves while the look-back still lists a comment files no second version of it and starts no second build.
func TestCommentKeepsTheHeadItWasFirstSeenAt(t *testing.T) {
	var head, comments atomic.Value

	head.Store("aaaa")
	comments.Store([]int{2, 1})

	cfg := commentedPR(t, &head, &comments)
	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}

	first := filedCommands(t, st)
	if strings.Count(first, `"sha":"aaaa"`) != 2 {
		t.Fatalf("filed %s, want both comments at aaaa", first)
	}

	head.Store("bbbb")

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil || len(enqueued) != 0 {
		t.Errorf("after the head moved: enqueued %v, %v; want nothing, the comments are the ones already built", enqueued, err)
	}

	if got := filedCommands(t, st); got != first {
		t.Errorf("filed %s, want %s unchanged", got, first)
	}

	comments.Store([]int{3, 2, 1})

	enqueued, err = pollOnce(ctx, cfg, st)
	if err != nil || len(enqueued) != 1 {
		t.Errorf("a new comment: enqueued %v, %v; want [act]", enqueued, err)
	}

	if got := filedCommands(t, st); strings.Count(got, `"sha":"aaaa"`) != 2 || strings.Count(got, `"sha":"bbbb"`) != 1 {
		t.Errorf("filed %s, want the new comment alone at bbbb", got)
	}
}

// TestCommentsFiledBeforeVersionsHeldACommitAreNotNews: the first check after an upgrade finds history without a sha, and reporting those comments with one would build every command in the look-back again.
func TestCommentsFiledBeforeVersionsHeldACommitAreNotNews(t *testing.T) {
	var head, comments atomic.Value

	head.Store("aaaa")
	comments.Store([]int{2, 1})

	cfg := commentedPR(t, &head, &comments)
	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	old := func(id int) map[string]any {
		return map[string]any{"id": strconv.Itoa(id), "kind": "conversation", "number": "42", "updated": fmt.Sprintf("2026-11-01T00:00:0%dZ", id)}
	}

	_, err := st.RecordVersions(ctx, "command", []map[string]any{old(1), old(2)}, 0)
	if err != nil {
		t.Fatal(err)
	}

	cursor, err := store.EncodeVersion(old(2))
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordCheckedVersion(ctx, "command", cursor)
	if err != nil {
		t.Fatal(err)
	}

	before := filedCommands(t, st)

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil || len(enqueued) != 0 {
		t.Errorf("enqueued %v, %v; want nothing", enqueued, err)
	}

	if got := filedCommands(t, st); got != before {
		t.Errorf("filed %s, want %s: the versions as they were filed", got, before)
	}
}
