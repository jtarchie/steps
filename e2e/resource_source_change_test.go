package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// movedResourcePipeline is a resource whose versions exist at ONE location, as an image digest exists in one registry: a check handed a cursor from somewhere else fails, and so does a get of a version from somewhere else.
func movedResourcePipeline(at, log string, trigger bool) string {
	return fmt.Sprintf(`defaults:
  preflight:
    disabled: true
resource_types:
- name: located
  config:
    check: |
      cursor='{{ if .version }}{{ .version.d }}{{ end }}'
      case "$cursor" in
        ""|{{ .source.at }}-*) echo '[{"d": "{{ .source.at }}-1"}]' ;;
        *) echo "$cursor does not exist at {{ .source.at }}" >&2; exit 1 ;;
      esac
    in: |
      case '{{ .version.d }}' in
        {{ .source.at }}-*) echo '{{ .version.d }}' >> %[2]s ;;
        *) echo '{{ .version.d }} does not exist at {{ .source.at }}' >&2; exit 1 ;;
      esac
resources:
- name: tools
  type: located
  source: {at: %[1]s}
jobs:
- name: build
  plan:
  - get: tools
    trigger: %[3]t
`, at, log, trigger)
}

// steps#280: a resource moved to a new source: is a different resource with its own history, as Concourse keys versions to the resource's type and source. The first run after the move checks the new location and fetches from it, instead of resolving a version recorded for the old one.
func TestChangingASourceStartsAFreshVersionHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "fetched.log")
	path := writePipeline(t, dir, movedResourcePipeline("old", log, false))

	err := stepsRun(t, []string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	writePipeline(t, dir, movedResourcePipeline("new", log, false))

	err = stepsRun(t, []string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("the first run after the source moved failed: %v", err)
	}

	if got := strings.Fields(readFileString(t, log)); strings.Join(got, ",") != "old-1,new-1" {
		t.Fatalf("fetched %v, want old-1 then new-1", got)
	}
}

// The daemon half of steps#280, where it was seen: a set that moves a resource polls the new source from scratch and triggers on what it finds there, rather than handing the old source's cursor to a check that cannot answer it.
func TestASetThatMovesAResourcePollsTheNewSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "fetched.log")
	path := writePipeline(t, dir, movedResourcePipeline("old", log, true))

	served := startWebFor(t, path, "--interval", "100ms")
	defer served.stop(t)

	waitForFetched(t, log, "old-1")

	writePipeline(t, dir, movedResourcePipeline("new", log, true))
	served.set(t, cli.PipelineName(path), path)

	waitForFetched(t, log, "old-1,new-1")
}

// As the issue saw it: the pipeline is set, then a job is run before the next poll. The set alone has to have moved the resource to a fresh history, or the run refreshes from the old source's cursor and fetches a version that only exists there.
func TestARunRightAfterASetFetchesFromTheNewSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "fetched.log")
	path := writePipeline(t, dir, movedResourcePipeline("old", log, true))

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	waitForFetched(t, log, "old-1")

	writePipeline(t, dir, movedResourcePipeline("new", log, true))
	served.set(t, cli.PipelineName(path), path)
	served.trigger(t, cli.PipelineName(path), "build")

	waitForFetched(t, log, "old-1,new-1")
}

func waitForFetched(t *testing.T, log, want string) {
	t.Helper()

	var got string

	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		body, _ := os.ReadFile(log) //nolint:gosec // a t.TempDir() file the pipeline under test writes
		if got = strings.Join(strings.Fields(string(body)), ","); got == want {
			return
		}
	}

	t.Fatalf("fetched %q, want %q", got, want)
}
