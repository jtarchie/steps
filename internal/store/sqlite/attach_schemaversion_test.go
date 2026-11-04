package sqlite

import (
	"errors"
	"path/filepath"
	"strings"
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

// Attach is a writable handle on a pipeline already in the file and registers nothing: the named pipeline opens, an unknown one is refused by name (a write that must not resurrect a destroyed pipeline).
func TestAttachOpensAKnownPipelineAndRefusesAnUnknownOne(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	st, err := OpenStore(path, "app")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	_ = st.Close()

	attached, err := Attach(path, "app")
	if err != nil {
		t.Fatalf("Attach to a known pipeline: %v", err)
	}

	_ = attached.Close()

	_, err = Attach(path, "ghost")
	if !errors.Is(err, store.ErrNoSuchPipeline) || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("Attach to an unknown pipeline = %v, want ErrNoSuchPipeline naming it", err)
	}
}
