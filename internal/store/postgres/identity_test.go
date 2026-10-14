package postgres

import "testing"

// The sqlite driver's TestIdentityIsMintedOnceAndKept, against Postgres.
func TestIdentityIsMintedOnceAndKept(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)

	first, err := Identity(t.Context(), rawURL)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}

	if len(first) != 16 {
		t.Errorf("Identity = %q, want 16 hex characters", first)
	}

	st, err := OpenStore(rawURL, "p")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	_ = st.Close()

	again, err := Identity(t.Context(), rawURL)
	if err != nil {
		t.Fatalf("Identity again: %v", err)
	}

	if again != first {
		t.Errorf("Identity after a reopen = %q, want %q", again, first)
	}

	other, err := Identity(t.Context(), newDatabase(t))
	if err != nil {
		t.Fatalf("Identity of another database: %v", err)
	}

	if other == first {
		t.Errorf("two databases share identity %q", first)
	}
}
