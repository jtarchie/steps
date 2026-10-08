package resource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestSlackMentionsUserFromAnOlderVersion: a version recorded before it
// carried the author still delivers one, found in the thread by the mention's
// own ts — the reply's author, never the thread's. A version neither knows
// writes an empty file, which a memory: step refuses.
//
// Not t.Parallel(): the type reads its token through env(), which is the
// process environment.
func TestSlackMentionsUserFromAnOlderVersion(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"101.000","user":"U2"},{"ts":"101.500","user":"U3"}]}`))
	}))
	t.Cleanup(server.Close)

	rt, err := config.ReadBuiltinResourceType("slack-mentions")
	if err != nil {
		t.Fatal(err)
	}

	for version, want := range map[[3]string]string{
		{"101.500", "101.000", ""}:   "U3",
		{"101.500", "101.000", "U9"}: "U9",
		{"999.000", "101.000", ""}:   "",
	} {
		dir := t.TempDir()

		fields := map[string]any{"channel": "C1", "ts": version[0], "thread_ts": version[1]}
		if version[2] != "" {
			fields["user"] = version[2]
		}

		err := RunIn(context.Background(), nil, rt, nil, map[string]any{"base_url": server.URL}, fields, nil, dir)
		if err != nil {
			t.Fatalf("RunIn(%v): %v", fields, err)
		}

		got, err := os.ReadFile(filepath.Join(dir, "user")) //nolint:gosec // t.TempDir
		if err != nil {
			t.Fatal(err)
		}

		if string(got) != want {
			t.Errorf("version %v wrote user %q, want %q", fields, got, want)
		}
	}
}
