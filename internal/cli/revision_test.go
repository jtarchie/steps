package cli

// What `steps runs` records and prints about the configuration a run
// executed. The end-to-end half of this — three runs, two configurations,
// the listing telling them apart — lives in ./e2e; these are the two
// branches no run reaches: a Config built in memory, and a hash short
// enough to print whole.

import (
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// TestRecordRevisionSkipsAConfigThatWasNeverLoaded covers the branch every
// e2e takes the other side of: a Config built in memory has no source
// to hash, and recording one anyway would write a row describing nothing.
func TestRecordRevisionSkipsAConfigThatWasNeverLoaded(t *testing.T) {
	t.Parallel()

	st, err := store.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	defer func() { _ = st.Close() }()

	err = RecordRevision(t.Context(), st, &config.Config{})
	if err != nil {
		t.Fatalf("recording a config that was never loaded: %v", err)
	}

	err = st.StartRun(t.Context(), "run-one", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows, err := st.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 || rows[0].ConfigSHA != "" {
		t.Fatalf("got %+v, want one run reporting no configuration", rows)
	}

	// And that is what the column prints, which is the claim docs/README.md
	// makes about a dash.
	if got := shortConfig(rows[0].ConfigSHA); got != "-" {
		t.Errorf("a run with no configuration prints %q, want %q", got, "-")
	}
}

// TestShortConfigKeepsAHashThatAlreadyFits pins the third branch: a hash
// shorter than the column is printed whole rather than sliced, which is what
// a naive prefix would panic on.
func TestShortConfigKeepsAHashThatAlreadyFits(t *testing.T) {
	t.Parallel()

	if got := shortConfig("abc123"); got != "abc123" {
		t.Errorf("shortConfig(%q) = %q, want it printed whole", "abc123", got)
	}
}
