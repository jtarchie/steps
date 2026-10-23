package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndASecondGetOfOneResourceFetchesItsOwnVersion: two gets under one name with different version: pins are two resolutions, and the later one's artifact is what the task reads, as in Concourse. The resolver cache was keyed by the get's name alone, so the second get was handed the first's version.
func TestEndToEndASecondGetOfOneResourceFetchesItsOwnVersion(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "seen.txt")

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: |
      printf '[{"n": "1"}, {"n": "2"}, {"n": "3"}]'
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: r
  type: counter
  source: {}
jobs:
- name: j
  plan:
  - get: r
    version: { n: "1" }
  - get: r
    version: { n: "3" }
  - task: show
    inputs: [r]
    run: cat r/n.txt > %s
`, out))

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	seen, err := os.ReadFile(out) //nolint:gosec // a t.TempDir() file this test named
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(string(seen)); got != "3" {
		t.Errorf("the task read version %q of r, want 3 (the later get's pin)", got)
	}
}
