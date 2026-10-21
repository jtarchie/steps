package sqlite

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestAttachRefusesANewerSchema: acquirePools leaves a newer file for checkSchemaVersion to judge, and Attach never runs it, so a write-back would land in a schema this build does not understand.
func TestAttachRefusesANewerSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	st, err := OpenStore(path, "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	_ = st.Close()

	stampVersion(t, path, schemaVersion+1)

	_, err = Attach(path, "test")
	if !errors.Is(err, store.ErrSchemaVersion) {
		t.Fatalf("Attach error = %v, want a schema-version refusal", err)
	}
}
