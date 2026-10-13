package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndTemplatesCannotReadDaemonEnv pins that a resource command's
// template has no way to reach the environment of the process rendering it.
// Resource commands render in the daemon, and a pipeline arrives over HTTP, so
// sprig's env/expandenv would hand anyone who can set a pipeline the daemon's
// credentials.
func TestEndToEndTemplatesCannotReadDaemonEnv(t *testing.T) {
	const secret = "s3cr3t-from-the-daemon"

	t.Setenv("STEPS_TEST_TEMPLATE_SECRET", secret)

	for _, expr := range []string{
		`env "STEPS_TEST_TEMPLATE_SECRET"`,
		`expandenv "$STEPS_TEST_TEMPLATE_SECRET"`,
	} {
		t.Run(strings.Fields(expr)[0], func(t *testing.T) {
			dir := t.TempDir()
			leak := filepath.Join(dir, "leak.log")

			pipeline := fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo '{{ %s }}' >> %s

resources:
- name: repo
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: repo
`, expr, leak)

			err := cli.Run([]string{writePipeline(t, dir, pipeline)})
			if err == nil {
				t.Fatal("run succeeded; a template calling a host-env function must fail")
			}

			if want := fmt.Sprintf("function %q not defined", strings.Fields(expr)[0]); !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to contain %q", err, want)
			}

			assertNotLeaked(t, leak, secret)
		})
	}
}

func assertNotLeaked(t *testing.T, path, secret string) {
	t.Helper()

	if !fileExists(path) {
		return
	}

	if strings.Contains(readFileString(t, path), secret) {
		t.Errorf("%s contains the daemon's secret", filepath.Base(path))
	}
}
