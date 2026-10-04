package venue

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/testsshd"
	"github.com/jtarchie/steps/internal/treedigest"
)

// scopeEviction keeps eviction to this process's volumes, since other test processes share the daemon, and sets the bound.
func scopeEviction(t *testing.T, bound int64, age time.Duration) {
	t.Helper()

	scope, oldBytes, oldAge := evictScope, cacheBytes, orphanAge
	evictScope = map[string]string{"steps.pid": strconv.Itoa(os.Getpid())}
	cacheBytes, orphanAge = bound, age

	t.Cleanup(func() { evictScope, cacheBytes, orphanAge = scope, oldBytes, oldAge })
}

func volumeExists(t *testing.T, name string) bool {
	t.Helper()

	return exec.CommandContext(t.Context(), "docker", "volume", "inspect", name).Run() == nil //nolint:gosec // a name this test built
}

func aliasOf(t *testing.T, cwd string) string {
	t.Helper()

	digest, err := treedigest.Tree(filepath.Join(cwd, "src"))
	if err != nil {
		t.Fatal(err)
	}

	return aliasPrefix + digest
}

func TestDockerPlusEvictsTheOldestPastTheBound(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)
	scopeEviction(t, 8<<30, time.Hour)

	worker := dockerPlusURL(testsshd.New(t), socket)

	cwds := make([]string, 0, 3)

	for range 3 {
		cwd := payloadDir(t, 4096)
		runAndClose(t, worker, cwd, "true")
		cwds = append(cwds, cwd)
		// CreatedAt has second resolution on some daemons; order must be unambiguous.
		time.Sleep(1100 * time.Millisecond)
	}

	newest := aliasOf(t, cwds[2])
	size := sizeOf(inspect(t, newest))

	if size == 0 {
		t.Fatal("the alias carries no size, so nothing can ever be evicted")
	}

	// Room for exactly one: the next session's eviction keeps the newest.
	scopeEviction(t, size, time.Hour)
	runAndClose(t, worker, payloadDir(t, 4096), "true")

	for i, cwd := range cwds[:2] {
		if volumeExists(t, aliasOf(t, cwd)) {
			t.Errorf("entry %d survived past the bound", i)
		}
	}

	if !volumeExists(t, newest) {
		t.Error("the newest entry was evicted before older ones")
	}
}

// Docker tracks no dependency between volumes, so removing a lower under a mounted overlay would succeed; eviction must read the child's label instead.
func TestDockerPlusNeverEvictsALowerInUse(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)
	scopeEviction(t, 8<<30, time.Hour)

	worker := dockerPlusURL(testsshd.New(t), socket)
	cwd := payloadDir(t, 4096)

	open := plusRunnerFor(t, worker, cwd)

	err := open.Run(t.Context(), "true")
	if err != nil {
		t.Fatal(err)
	}

	scopeEviction(t, 1, time.Hour)
	runAndClose(t, worker, payloadDir(t, 4096), "true")

	err = open.Run(t.Context(), "test -s src/blob.bin")
	if err != nil {
		t.Fatalf("the open session lost its input to eviction: %v", err)
	}

	if !volumeExists(t, aliasOf(t, cwd)) {
		t.Fatal("an entry an open session is layered on was evicted")
	}
}

func TestDockerPlusReclaimsOrphansButNotWhatIsMounted(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)

	labels := make([]string, 0, 6)
	labels = append(labels, "--label", "steps.owner=steps", "--label", "steps.pid="+strconv.Itoa(os.Getpid()))
	orphan, mounted := "steps-d-orphan"+randomSuffix(), "steps-work-mounted"+randomSuffix()

	for _, name := range []string{orphan, mounted} {
		err := exec.CommandContext(t.Context(), "docker", append([]string{"volume", "create"}, append(labels, name)...)...).Run() //nolint:gosec // names this test built
		if err != nil {
			t.Fatal(err)
		}

		// Not t.Context(): it is already cancelled when cleanups run.
		t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "volume", "rm", name).Run() }) //nolint:gosec // as above
	}

	container := "steps-test-mounting-" + randomSuffix()

	err := exec.CommandContext(t.Context(), "docker", "run", "-d", "--name", container, "-v", mounted+":/m", "alpine:3", "sleep", "60").Run() //nolint:gosec // names this test built
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "rm", "-f", "-v", container).Run() }) //nolint:gosec // as above

	worker := dockerPlusURL(testsshd.New(t), socket)
	cached := payloadDir(t, 1024)
	runAndClose(t, worker, cached, "true")

	scopeEviction(t, 8<<30, 0)
	runAndClose(t, worker, payloadDir(t, 1024), "true")

	if !volumeExists(t, aliasOf(t, cached)) || !volumeExists(t, dataOf(t, aliasOf(t, cached))) {
		t.Error("a cache entry an alias names was reclaimed as an orphan")
	}

	if volumeExists(t, orphan) {
		t.Error("an orphaned data volume was not reclaimed")
	}

	if !volumeExists(t, mounted) {
		t.Error("a volume a running container mounts was reclaimed")
	}
}

func inspect(t *testing.T, name string) dockerapi.Volume {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "docker", "volume", "inspect", "--format", "{{index .Labels \""+cacheSize+"\"}}", name).Output() //nolint:gosec // a name this test built
	if err != nil {
		t.Fatalf("inspecting %s: %v", name, err)
	}

	return dockerapi.Volume{Labels: map[string]string{cacheSize: string(out[:len(out)-1])}}
}

func dataOf(t *testing.T, alias string) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "docker", "volume", "inspect", "--format", "{{index .Labels \""+cacheData+"\"}}", alias).Output() //nolint:gosec // a name this test built
	if err != nil {
		t.Fatalf("inspecting %s: %v", alias, err)
	}

	return string(out[:len(out)-1])
}
