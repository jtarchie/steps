package e2e

// A worker's artifact cache after a temp cleaner has been through it (steps#119).

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndWorkerRefetchesAnInputATempCleanerHollowed: a cleaner ages files, not directories, so it empties a cache entry and leaves it standing under its digest — which the worker used to place as the whole artifact, running the step without its input on every run until the entry was evicted.
func TestEndToEndWorkerRefetchesAnInputATempCleanerHollowed(t *testing.T) {
	token := fmt.Sprintf("hollow-%d", time.Now().UnixNano())

	// Two copies of one pipeline: separate state databases, so the second run's step cache cannot skip the placed step, while both send the same input under the same digest.
	pipeline := func(dir string) string {
		return writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    outputs: [data]
    run: |
      mkdir -p data/deep
      echo `+token+` > data/deep/kept.txt
      echo `+token+` > data/gone.txt
  - task: consume
    tags: [gpu]
    inputs: [data]
    outputs: [out]
    run: cat data/deep/kept.txt data/gone.txt > out/both.txt
  - task: publish
    inputs: [out]
    run: cp out/both.txt `+filepath.Join(dir, "published.txt")+`
`)
	}

	err := cli.Run([]string{pipeline(t.TempDir()), "--worker", "gpu=local:"})
	if err != nil {
		t.Fatalf("the first run failed: %v", err)
	}

	if hollowCachedInput(t, token, "gone.txt") == 0 {
		t.Fatal("no cache entry on the worker holds the input, so the second run would reuse nothing and this test would prove nothing")
	}

	second := t.TempDir()

	err = cli.Run([]string{pipeline(second), "--worker", "gpu=local:"})
	if err != nil {
		t.Fatalf("the second run failed: %v\n\nthe worker placed a cache entry a temp cleaner had emptied, and the step ran without its input", err)
	}

	want := token + "\n" + token + "\n"
	if got := readFileString(t, filepath.Join(second, "published.txt")); got != want {
		t.Errorf("published = %q, want %q", got, want)
	}
}

// hollowCachedInput deletes each file called name holding token from the local: worker's cache, leaving its directories; the token is unique to the caller, so no other test's entries are touched.
func hollowCachedInput(t *testing.T, token, name string) int {
	t.Helper()

	hollowed := 0

	_ = filepath.WalkDir(filepath.Join(os.TempDir(), "steps-shim", "artifacts"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != name {
			return nil //nolint:nilerr // other tests share this cache and evict from it mid-walk
		}

		content, readErr := os.ReadFile(path) //nolint:gosec // a file under the worker cache this test filled
		if readErr != nil || strings.TrimSpace(string(content)) != token {
			return nil //nolint:nilerr // not this test's entry
		}

		if os.Remove(path) == nil { //nolint:gosec // a file under the worker cache this test filled
			hollowed++
		}

		return nil
	})

	return hollowed
}
