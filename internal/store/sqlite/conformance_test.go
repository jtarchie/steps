package sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/storetest"
)

// TestStoreConformance runs the store's conformance suite against this driver.
//
// Everything it asserts is written against internal/store's contract, so a
// second driver proves itself by adding one test exactly like this one. What
// stays in this package is what measures SQLite itself — the file footprint,
// the pragmas, the schema stamp — and nothing a caller of store.Store can see.
func TestStoreConformance(t *testing.T) {
	t.Parallel()

	// One database per test, shared by every pipeline name that test opens:
	// the multi-pipeline cases are about two handles on ONE file, and a
	// t.TempDir() per call would hand them two.
	root := t.TempDir()

	storetest.Run(t, func(t *testing.T, pipeline string) store.Store {
		t.Helper()

		path := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "-"), "state.db")

		st, err := OpenStore(path, pipeline)
		if err != nil {
			t.Fatalf("OpenStore(%q, %q): %v", path, pipeline, err)
		}

		return st
	})
}
