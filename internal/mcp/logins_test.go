package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

const testPipeline = "app"

// useConfigDir points os.UserConfigDir at a fresh directory and returns the
// root every pipeline's logins live under.
func useConfigDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	root, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}

	return filepath.Join(root, "steps", "mcp")
}

func TestTokenPathIsOnePipelinesServer(t *testing.T) {
	root := useConfigDir(t)

	path, err := TokenPath("app", "tracker")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	if want := filepath.Join(root, "app", "tracker.json"); path != want {
		t.Fatalf("TokenPath = %q, want %q", path, want)
	}

	upper, err := TokenPath("App", "tracker")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	// A case-insensitive filesystem would fold these into one directory, which is two pipelines sharing a login.
	if strings.EqualFold(upper, path) {
		t.Fatalf("TokenPath(App) = %q folds into TokenPath(app) = %q", upper, path)
	}

	for _, name := range []string{"a/b", ".", "..", "../x", `a\b`, strings.Repeat("x", 300)} {
		staysOneSegment(t, root, name)
	}

	for _, names := range [][2]string{{"", "tracker"}, {"app", ""}} {
		_, err := TokenPath(names[0], names[1])
		if err == nil {
			t.Errorf("TokenPath(%q, %q) succeeded, want refused", names[0], names[1])
		}
	}
}

func staysOneSegment(t *testing.T, root, name string) {
	t.Helper()

	got, err := TokenPath(name, "tracker")
	if err != nil {
		t.Fatalf("TokenPath(%q): %v", name, err)
	}

	rel, err := filepath.Rel(root, got)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}

	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "." || parts[0] == ".." || parts[1] != "tracker.json" {
		t.Errorf("TokenPath(%q) = %q, want one segment inside %q", name, got, root)

		return
	}

	if len(parts[0]) >= 255 {
		t.Errorf("TokenPath(%q) segment is %d bytes, over a filesystem's name limit", name, len(parts[0]))
	}
}

func TestForgetLoginsTouchesOnlyItsPipeline(t *testing.T) {
	root := useConfigDir(t)

	for _, pipeline := range []string{"app", "other"} {
		writeLogin(t, pipeline, "tracker")
	}

	err := ForgetLogins("")
	if err == nil {
		t.Fatal("ForgetLogins(\"\") succeeded, want refused")
	}

	err = ForgetLogins("app")
	if err != nil {
		t.Fatalf("ForgetLogins: %v", err)
	}

	_, err = os.Stat(filepath.Join(root, "app"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("app's logins survived ForgetLogins: %v", err)
	}

	_, err = os.Stat(filepath.Join(root, "other", "tracker.json"))
	if err != nil {
		t.Errorf("other's login was removed with app's: %v", err)
	}
}

func TestMoveLoginsRefusesAnExistingDestination(t *testing.T) {
	root := useConfigDir(t)

	err := MoveLogins("absent", "anywhere")
	if err != nil {
		t.Fatalf("MoveLogins with no logins: %v", err)
	}

	_, err = os.Stat(filepath.Join(root, "anywhere"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a move with no logins created the destination: %v", err)
	}

	writeLogin(t, "app", "tracker")

	err = os.MkdirAll(filepath.Join(root, "taken"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = MoveLogins("app", "taken")
	if !errors.Is(err, ErrLoginsExist) {
		t.Fatalf("MoveLogins onto an empty existing directory = %v, want ErrLoginsExist", err)
	}

	err = MoveLogins("app", "app2")
	if err != nil {
		t.Fatalf("MoveLogins: %v", err)
	}

	_, err = os.Stat(filepath.Join(root, "app2", "tracker.json"))
	if err != nil {
		t.Errorf("the login did not move: %v", err)
	}

	if dir, found := LoginsExist("app"); found {
		t.Errorf("the old directory %s survived the move", dir)
	}
}

func TestARefreshAfterTheLoginMovedDoesNotRecreateIt(t *testing.T) {
	ts, _ := tokenEndpointServer(t)
	root := useConfigDir(t)

	srv := config.MCPServer{Name: "linear", Endpoint: "https://mcp.linear.app/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}

	path, err := TokenPath(testPipeline, srv.Name)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	tf := &TokenFile{
		Endpoint:     srv.Endpoint,
		ClientID:     "client-id",
		TokenURL:     ts.URL,
		AccessToken:  "stale-access-token",
		RefreshToken: "stale-refresh-token",
		Expiry:       time.Now().Add(-time.Hour),
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	source, err := oauthTokenSource(context.Background(), testPipeline, srv)
	if err != nil {
		t.Fatalf("oauthTokenSource: %v", err)
	}

	err = ForgetLogins(testPipeline)
	if err != nil {
		t.Fatalf("ForgetLogins: %v", err)
	}

	refreshAndCheck(t, source, "access-token-call-1")

	_, err = os.Stat(filepath.Join(root, testPipeline))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refresh recreated the removed login directory: %v", err)
	}
}

func TestAFlatLoginFromBeforePipelinesNeedsLogin(t *testing.T) {
	root := useConfigDir(t)

	srv := config.MCPServer{Name: "tracker", Endpoint: inspectEndpoint, Auth: config.MCPServerAuth{Type: "oauth"}}

	flat := &TokenFile{Endpoint: srv.Endpoint, AccessToken: "old"}

	err := flat.Save(filepath.Join(root, "tracker.json"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := InspectToken(testPipeline, srv)
	if got.Connected || got.Detail != "needs login" {
		t.Fatalf("InspectToken with only a flat file = %+v, want needs login", got)
	}
}

func writeLogin(t *testing.T, pipeline, server string) {
	t.Helper()

	path, err := TokenPath(pipeline, server)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	tf := &TokenFile{Endpoint: "https://example.test/mcp", AccessToken: "token"}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// Kept verbatim, a pipeline called `tracker.json` would be the pre-pipeline login FILE for server tracker, where a directory is expected.
func TestAPipelineNamedLikeAFlatLoginGetsItsOwnDirectory(t *testing.T) {
	root := useConfigDir(t)

	got, err := TokenPath("tracker.json", "tracker")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	if dir := filepath.Dir(got); dir == filepath.Join(root, "tracker.json") {
		t.Fatalf("TokenPath(tracker.json) = %q, the path of a pre-pipeline login file", got)
	}
}

// A login directory that cannot be looked at is not known to be absent, and a rename onto it must not go ahead as though it were.
func TestLoginsExistWhenItCannotLook(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through a mode-000 directory")
	}

	root := useConfigDir(t)

	err := os.MkdirAll(root, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Chmod(root, 0o000)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) //nolint:gosec // a directory needs its execute bit to be removable

	if dir, found := LoginsExist("app"); !found {
		t.Errorf("LoginsExist(%s) behind an unreadable directory = absent, want treated as present", dir)
	}
}
