package blobstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// TestGetTreeRefusesABlobPastItsLimits: a bucket is writable by more than
// this orchestrator, and a blob is unpacked before its digest can be checked —
// so what it may cost the disk is bounded while it lands, not after.
func TestGetTreeRefusesABlobPastItsLimits(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "result.txt"), []byte("payload\n"), 0o600)
	if err != nil {
		t.Fatalf("writing the tree: %v", err)
	}

	err = store.PutTree(ctx, "big", src)
	if err != nil {
		t.Fatalf("PutTree: %v", err)
	}

	store.opts.Limits = wire.UnpackOptions{MaxBytes: 4}

	err = store.GetTree(ctx, "big", filepath.Join(t.TempDir(), "restored"))
	if !errors.Is(err, wire.ErrTreeTooLarge) {
		t.Fatalf("GetTree = %v, want wire.ErrTreeTooLarge", err)
	}
}

// TestNewBoundsWhatGetTreeUnpacks: Options with no limits means the fetch
// defaults, never no limit — unlimited is not a thing a bucket reader wants.
func TestNewBoundsWhatGetTreeUnpacks(t *testing.T) {
	store, _ := newTestStore(t)

	if store.opts.Limits != wire.FetchLimits() {
		t.Fatalf("store limits = %+v, want wire.FetchLimits() %+v", store.opts.Limits, wire.FetchLimits())
	}
}
