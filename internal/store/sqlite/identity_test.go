package sqlite

import (
	"path/filepath"
	"testing"
)

// The id names this database's daemon on every machine it tags (#120), so it must survive a restart and differ between databases, or two daemons would claim each other's machines.
func TestIdentityIsMintedOnceAndKept(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "steps.db")

	first, err := Identity(t.Context(), path)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}

	if len(first) != 16 {
		t.Errorf("Identity = %q, want 16 hex characters", first)
	}

	st, err := OpenStore(path, "p")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	_ = st.Close()

	again, err := Identity(t.Context(), path)
	if err != nil {
		t.Fatalf("Identity again: %v", err)
	}

	if again != first {
		t.Errorf("Identity after a reopen = %q, want %q", again, first)
	}

	other, err := Identity(t.Context(), filepath.Join(t.TempDir(), "steps.db"))
	if err != nil {
		t.Fatalf("Identity of another database: %v", err)
	}

	if other == first {
		t.Errorf("two databases share identity %q", first)
	}
}
