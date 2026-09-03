package e2e

// Where the repository is, from here.
//
// These tests run in ./e2e but assert about files at the module root — the
// published schema, the example corpus, the docs index — so every such path
// is built from repoRoot rather than written relative. A test that reads
// "steps.schema.json" would pass in the root package and silently find
// nothing here.

import "path/filepath"

// repoRoot is the module root relative to this package's directory, which is
// what `go test` makes the working directory.
const repoRoot = ".."

// repoFile joins a path at the module root.
func repoFile(parts ...string) string {
	return filepath.Join(append([]string{repoRoot}, parts...)...)
}
