package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestForceDoesNotReplayTakenVersions pins #145: --force skips the step cache
// and nothing more. The put is the point — the cache never skips an effect, so
// a replayed version shows up as an extra line.
func TestForceDoesNotReplayTakenVersions(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	published := filepath.Join(dir, "published.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: ticker
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt
- name: recorder
  config:
    check: echo '[]'
    in: 'true'
    out: echo {{ .params.n | shellquote }} >> %[2]s

resources:
- name: ticks
  type: ticker
  source: {}
- name: log
  type: recorder
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - put: log
    params: {n: x}
`, versions, published))

	run := func(args ...string) string {
		return captureStdout(t, func() {
			err := cli.Run(append([]string{"run", path, "--job", "build"}, args...))
			if err != nil {
				t.Fatalf("run %v: %v", args, err)
			}
		})
	}

	writePipelineFile(t, versions, `[{"n":"one"}]`)
	run()
	assertLineCount(t, published, 1)

	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)
	run("--force")
	assertLineCount(t, published, 2)

	out := run("--force")
	assertLineCount(t, published, 2)

	for _, want := range []string{"already taken", "--force skips the step cache"} {
		if !strings.Contains(out, want) {
			t.Errorf("forced idle run output lacks %q:\n%s", want, out)
		}
	}

	// The forced run recorded "two", so an ordinary run has nothing either.
	run()
	assertLineCount(t, published, 2)
}

// TestStepsTestRerunsEveryVersionsDeterministically: `steps test` re-opens
// taken versions so its execution assertions hold on every rerun against one
// state file, which --force alone no longer guarantees.
func TestStepsTestRerunsEveryVersionsDeterministically(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
resource_types:
- name: ticker
  config:
    check: printf '[{"n":"1"}]'
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: ticker
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - task: work
    run: 'true'
  assert:
    execution: [ticks, work]
`)

	for i := range 2 {
		err := cli.Run([]string{"test", path})
		if err != nil {
			t.Fatalf("steps test, run %d: %v", i+1, err)
		}
	}
}
