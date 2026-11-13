package e2e

// The default-path bench (steps#246): what an untagged image: step costs through today's bind mount against the same step on a local: worker's volumes, so the switch is measured before the bind path is deleted. `task bench:default-path` runs it.

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

func TestBenchDefaultPath(t *testing.T) {
	if os.Getenv("STEPS_BENCH_DEFAULT_PATH") == "" {
		t.Skip("the default-path bench runs only on request: task bench:default-path")
	}

	requireDockerE2E(t)

	files, size := benchEnvInt(t, "FILES", 100_000), benchEnvInt(t, "FILE_BYTES", 10_240)
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	benchSeedTree(t, seed, files, size)

	path := writePipeline(t, dir, benchDefaultPathPipeline())
	t.Logf("tree: %d files of %d bytes (%s)", files, size, formatBytes(int64(files*size)))

	var rows []string

	for _, job := range []string{"bind", "volume"} {
		for round := 1; round <= 2; round++ {
			var err error

			out := captureStdout(t, func() {
				err = cli.Run([]string{path, "--job", job, "--force", "--var", "seed=" + seed, "--worker", "v=local:"})
			})
			if err != nil {
				t.Fatalf("job %s round %d: %v\n%s", job, round, err, out)
			}

			walls := benchStepWalls(t, path)
			rows = append(rows, fmt.Sprintf("%-7s %-5s %10s %10s", job, cmp.Or(map[int]string{1: "first"}[round], "again"),
				walls["read"].Round(100*time.Millisecond), walls["rmw"].Round(100*time.Millisecond)))
		}
	}

	t.Log("\npath    round       read        rmw\n" + strings.Join(rows, "\n"))
}

// benchEnvInt reads a positive size from the environment, or takes the default.
func benchEnvInt(t *testing.T, name string, fallback int) int {
	t.Helper()

	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q: want a positive integer", name, raw)
	}

	return n
}

// benchSeedTree writes the same tree every time, a repository-shaped fan of small files, so a second round can hit a content-addressed cache.
func benchSeedTree(t *testing.T, root string, files, size int) {
	t.Helper()

	body := []byte(strings.Repeat("x", size))

	for i := range files {
		sub := filepath.Join(root, fmt.Sprintf("d%03d", i%1000))

		err := os.MkdirAll(sub, 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%06d", i)), body, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// benchStepWalls reads each step's wall time from the newest run's events: an untagged step records no placement to read it from.
func benchStepWalls(t *testing.T, path string) map[string]time.Duration {
	t.Helper()

	st := openStoreFor(t, path)
	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), "", 1)
	if err != nil || len(runs) == 0 {
		t.Fatalf("latest run: %v", err)
	}

	events, err := st.RunEvents(t.Context(), runs[0].ID, 0, 100_000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}

	walls := map[string]time.Duration{}

	for _, event := range events {
		if event.Type == "step_finished" {
			walls[event.StepName] = time.Duration(event.DurationMS) * time.Millisecond
		}
	}

	return walls
}

// benchDefaultPathPipeline is one job per path: seed copies the tree on the host, read walks it in a container, rmw changes one file of it as its declared output, which is what every write of an agent with outputs: [repo] costs.
func benchDefaultPathPipeline() string {
	job := func(name, tags string) string {
		return fmt.Sprintf(`- name: %[1]s
  plan:
  - task: seed
    outputs: [tree]
    run: cp -R "((seed))"/. tree/
  - task: read
    image: %[2]s%[3]s
    inputs: [tree]
    run: test "$(find tree -type f | wc -l)" -gt 0 && cat tree/d000/* > /dev/null
  - task: rmw
    image: %[2]s%[3]s
    inputs: [tree]
    outputs: [tree]
    run: echo changed >> tree/d000/f000000
`, name, benchImage, tags)
	}

	return "jobs:\n" + job("bind", "") + job("volume", "\n    tags: [v]")
}
