package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
)

// parseWeb parses argv as `steps web …` without serving, so a test reads the flags a config file set.
func parseWeb(t *testing.T, args ...string) (*WebCmd, error) {
	t.Helper()

	var cli CLI

	parser, err := kong.New(&cli, kong.Name("steps"), kong.Vars{"version": "test"})
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	_, err = parser.Parse(append([]string{"web"}, args...))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	return &cli.Web, nil
}

func configFileWith(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "steps.yml")
	err := os.WriteFile(path, []byte(body), mode)
	if err != nil {
		t.Fatal(err)
	}

	// WriteFile's mode is under the umask; the bits under test must be set for real.
	err = os.Chmod(path, mode)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// Pins the point #233 left unverified: a YAML map reaches a map[string]string flag and a YAML list a []string one, through kong's own decoders.
func TestConfigFileSetsEveryFlagShape(t *testing.T) {
	t.Parallel()

	path := configFileWith(t, `
web:
  listen: 0.0.0.0:9000
  interval: 10s
  max-concurrent: 3
  read-only: true
  pin: {number: "87", branch: main}
  role:
  - admin=Provider == "github"
  - viewer=true
  allowed-host: [ci.example.com, ci.internal]
  log-level: debug
`, 0o600)

	web, err := parseWeb(t, "--config", path, "--interval", "5s")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if web.Listen != "0.0.0.0:9000" {
		t.Errorf("listen = %q", web.Listen)
	}

	if web.Interval != 5*time.Second {
		t.Errorf("interval = %s, want the command line's 5s over the file's 10s", web.Interval)
	}

	assertCollectionFlags(t, web)
}

func assertCollectionFlags(t *testing.T, web *WebCmd) {
	t.Helper()

	if web.MaxConcurrent != 3 || !web.ReadOnly {
		t.Errorf("max-concurrent = %d, read-only = %v", web.MaxConcurrent, web.ReadOnly)
	}

	if web.Pin["number"] != "87" || web.Pin["branch"] != "main" {
		t.Errorf("pin = %v", web.Pin)
	}

	if len(web.Role) != 2 || web.Role[0] != `admin=Provider == "github"` {
		t.Errorf("role = %q", web.Role)
	}

	if strings.Join(web.AllowedHost, ",") != "ci.example.com,ci.internal" {
		t.Errorf("allowed-host = %q", web.AllowedHost)
	}
}

func TestConfigFileRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, body string
		mode       os.FileMode
		want       string
	}{
		{"unknown key", "web:\n  listne: x\n", 0o600, "unknown key web.listne"},
		{"top-level key", "run:\n  db: x\n", 0o600, `unknown key "run"`},
		{"password", "web:\n  basic-auth-password: hunter2\n", 0o600, "STEPS_BASIC_AUTH_PASSWORD"},
		{"config in config", "web:\n  config: other.yml\n", 0o600, "may not be set in the file"},
		{"group writable", "web: {}\n", 0o620, "writable by group or others"},
		{"not yaml", "web: [\n", 0o600, "yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := configFileWith(t, tc.body, tc.mode)

			_, err := parseWeb(t, "--config", path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}

			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the refusal repeated the password: %v", err)
			}
		})
	}
}

func TestConfigFileMustExist(t *testing.T) {
	t.Parallel()

	_, err := parseWeb(t, "--config", filepath.Join(t.TempDir(), "absent.yml"))
	if err == nil || !strings.Contains(err.Error(), "absent.yml") {
		t.Fatalf("got %v, want a refusal naming the file", err)
	}
}

// A YAML number under a map-of-strings flag is the likeliest authoring slip; the error must name the flag.
func TestConfigFileNamesTheFlagAWrongTypeWasGivenTo(t *testing.T) {
	t.Parallel()

	path := configFileWith(t, "web:\n  pin: {number: 87}\n", 0o600)

	_, err := parseWeb(t, "--config", path)
	if err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("got %v, want an error naming pin", err)
	}
}
