package main

// How a placement renders when the worker could not answer.

import (
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestPlacedFilesystemStatesItsSilence: an empty fstype is a shim that could
// not say — an older one, or a platform with no statfs — and never an
// ordinary disk. Rendering it blank hides exactly the answer this column
// exists to surface, since tmpfs is what a reader is scanning for.
func TestPlacedFilesystemStatesItsSilence(t *testing.T) {
	t.Parallel()

	if got := placedFilesystem(store.Placement{}); got != "not reported" {
		t.Errorf("placedFilesystem with no answer = %q, want a stated silence", got)
	}

	got := placedFilesystem(store.Placement{FSType: "tmpfs", FSFree: 848 << 20})
	if got != "tmpfs (848.0 MiB free)" {
		t.Errorf("placedFilesystem = %q, want the type and what is left on it", got)
	}
}

// TestPlacedMachineNamesTheImage, because a containerized placed step has two
// answers to "where did this run" and the image is the one that changes.
func TestPlacedMachineNamesTheImage(t *testing.T) {
	t.Parallel()

	bare := store.Placement{Address: "aws://i-0123456789abcdef0"}
	if got := placedMachine(bare); got != "aws://i-0123456789abcdef0" {
		t.Errorf("placedMachine = %q, want just the machine", got)
	}

	boxed := store.Placement{Address: "aws://i-0123456789abcdef0", Image: "golang:1.25"}
	if got := placedMachine(boxed); got != "aws://i-0123456789abcdef0 in golang:1.25" {
		t.Errorf("placedMachine = %q, want the machine and the image", got)
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	for _, want := range []struct {
		bytes int64
		text  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{67 << 20, "67.0 MiB"},
		{41_083_355_136, "38.3 GiB"},
		// Clamped at TiB rather than running off the unit table.
		{5 << 50, "5120.0 TiB"},
	} {
		if got := humanBytes(want.bytes); got != want.text {
			t.Errorf("humanBytes(%d) = %q, want %q", want.bytes, got, want.text)
		}
	}
}
