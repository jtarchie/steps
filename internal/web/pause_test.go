package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runnableServer is the ordinary daemon: one that holds a pipeline AND is
// allowed to act on it. testPipeline's server has no runner, which is
// --read-only, so every control test needs this instead.
func runnableServer(t *testing.T) (*Server, *Pipeline) {
	t.Helper()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipeline
}

// postForm returns the Location as well as the status, because where a
// control sends the reader back to is half of what it does.
func postForm(t *testing.T, server *Server, target string, form map[string]string) (int, string) {
	t.Helper()

	values := make([]string, 0, len(form))
	for key, value := range form {
		values = append(values, key+"="+value)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(strings.Join(values, "&")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Header().Get("Location")
}

// TestPauseIsAControlAndNotOnlyAState: every page of a paused pipeline said
// so and then told the reader to go find a terminal — the two verbs live on
// /api, which refuses a browser on purpose. The board offers the pause and
// the banner offers the release, and each puts the reader back where they
// were rather than on a page they did not ask for.
func TestPauseIsAControlAndNotOnlyAState(t *testing.T) {
	t.Parallel()

	server, pipeline := runnableServer(t)

	_, board := get(t, server, "/p/demo")
	if !strings.Contains(board, `action="/p/demo/pause"`) {
		t.Fatalf("the jobs board offers no way to pause the pipeline:\n%s", board)
	}

	code, where := postForm(t, server, "/p/demo/pause", map[string]string{"return": "/p/demo/runs"})
	if code != http.StatusSeeOther || where != "/p/demo/runs" {
		t.Fatalf("pause answered %d to %q, want 303 back to the page it was pressed on", code, where)
	}

	pausedIs(t, pipeline, true)

	// The board's own button is gone while it is paused: the banner carries
	// the other direction, and two controls claiming the same switch is how a
	// reader learns to distrust both.
	_, board = get(t, server, "/p/demo")
	if strings.Contains(board, `action="/p/demo/pause"`) {
		t.Error("a paused pipeline's board still offers to pause it")
	}

	if !strings.Contains(board, `action="/p/demo/unpause"`) {
		t.Errorf("a paused pipeline's page does not offer to resume it:\n%s", board)
	}

	code, _ = postForm(t, server, "/p/demo/unpause", nil)
	if code != http.StatusSeeOther {
		t.Fatalf("unpause answered %d, want 303", code)
	}

	pausedIs(t, pipeline, false)
}

// pausedIs asserts the stored breaker, which is the half of a control that outlives the redirect it answers with.
func pausedIs(t *testing.T, pipeline *Pipeline, want bool) {
	t.Helper()

	is, err := pipeline.Store.Paused(context.Background())
	if err != nil {
		t.Fatalf("Paused: %v", err)
	}

	if is != want {
		t.Fatalf("Paused = %v, want %v", is, want)
	}
}

// TestAControlReturnsOnlyToThisServer: the banner is on every page, so these
// forms carry where they came from — and a redirect built from what the page
// sent is a redirect somebody else can aim. "//host/path" is the one that
// does not look like a URL and is: browsers read it as absolute.
func TestAControlReturnsOnlyToThisServer(t *testing.T) {
	t.Parallel()

	server, _ := runnableServer(t)

	for _, probe := range []struct{ sent, want string }{
		{"/p/demo/runs", "/p/demo/runs"},
		{"//evil.tld/x", "/p/demo"},
		{`/\evil.tld/x`, "/p/demo"},
		{"https://evil.tld/x", "/p/demo"},
		{"", "/p/demo"},
	} {
		code, where := postForm(t, server, "/p/demo/pause", map[string]string{"return": probe.sent})
		if code != http.StatusSeeOther || where != probe.want {
			t.Errorf("a control asked to return to %q answered %d toward %q, want 303 toward %q",
				probe.sent, code, where, probe.want)
		}
	}
}

// TestAReadOnlyServerWithholdsThePauseControls: --read-only is a statement
// about the browser's surface, and pause is now part of that surface. The
// routes refuse and the buttons are not drawn — a button that 403s is worse
// than no button, since the reader cannot tell it from a broken daemon.
func TestAReadOnlyServerWithholdsThePauseControls(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	for _, target := range []string{"/p/demo/pause", "/p/demo/unpause"} {
		if code, _ := postForm(t, server, target, nil); code != http.StatusForbidden {
			t.Errorf("POST %s on a read-only server = %d, want 403", target, code)
		}
	}

	err := pipeline.Store.Pause(context.Background())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	_, board := get(t, server, "/p/demo")
	if strings.Contains(board, `action="/p/demo/unpause"`) {
		t.Error("a read-only server draws a control it refuses")
	}

	// Still says so, and still says how: the state is a diagnostic even where the remedy is not this server's to offer.
	if !strings.Contains(board, "steps pipeline unpause -p demo") {
		t.Errorf("a read-only server's paused page names no way to resume it:\n%s", board)
	}
}

// TestARebindingPageCannotPauseWhatItDidNotOpen: /api refuses every browser,
// so these routes are where a page can reach the breaker at all. They are
// POSTs under sameOriginMutations, which is the same guard trigger and abort
// have — and the reason adding pause to the UI grants a hostile page nothing:
// it could already press Trigger, which runs arbitrary commands.
func TestARebindingPageCannotPauseWhatItDidNotOpen(t *testing.T) {
	t.Parallel()

	server, pipeline := runnableServer(t)

	if code := postWithOrigin(t, server, "/p/demo/pause", "http://rebind.attacker.tld"); code != http.StatusForbidden {
		t.Errorf("a cross-origin pause answered %d, want 403", code)
	}

	pausedIs(t, pipeline, false)
}
