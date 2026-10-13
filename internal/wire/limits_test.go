package wire

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestUnpackRefusesATreePastItsLimits: a fetched tree is someone else's bytes
// landing on this machine's disk, and nothing about tar bounds how many.
func TestUnpackRefusesATreePastItsLimits(t *testing.T) {
	t.Parallel()

	archive := func(files ...string) *bytes.Buffer {
		var buf bytes.Buffer

		writer := tar.NewWriter(&buf)
		for _, name := range files {
			writeRegular(writer, name)
		}

		_ = writer.Close()

		return &buf
	}

	for _, tc := range []struct {
		name  string
		opts  UnpackOptions
		files []string
		want  error
	}{
		{"bytes past MaxBytes", UnpackOptions{MaxBytes: 7}, []string{"a", "b"}, ErrTreeTooLarge},
		{"entries past MaxEntries", UnpackOptions{MaxEntries: 2}, []string{"a", "b", "c"}, ErrTreeTooLarge},
		{"exactly MaxBytes", UnpackOptions{MaxBytes: 8}, []string{"a", "b"}, nil},
		{"exactly MaxEntries", UnpackOptions{MaxEntries: 3}, []string{"a", "b", "c"}, nil},
		{"zero is no limit", UnpackOptions{}, []string{"a", "b", "c"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, unpack := range []func(io.Reader, string, UnpackOptions) error{UnpackTree, UnpackFetchedTree} {
				err := unpack(archive(tc.files...), t.TempDir(), tc.opts)
				if !errors.Is(err, tc.want) {
					t.Fatalf("unpack = %v, want %v", err, tc.want)
				}
			}
		})
	}
}

// TestUnpackRefusesAnOversizedFileBeforeWritingIt: a header can declare more
// bytes than the cap allows, and the refusal must come before any of them land.
func TestUnpackRefusesAnOversizedFileBeforeWritingIt(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)
	_ = writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "big", Mode: 0o600, Size: 1 << 20, Format: tar.FormatPAX})
	_, _ = writer.Write(make([]byte, 1<<20))
	_ = writer.Close()

	root := t.TempDir()

	err := UnpackFetchedTree(&buf, root, UnpackOptions{MaxBytes: 1024})
	if !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("UnpackFetchedTree = %v, want ErrTreeTooLarge", err)
	}

	_, statErr := os.Lstat(filepath.Join(root, "big"))
	if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the oversized file was created anyway: %v", statErr)
	}
}

// TestUnpackRefusesASizeThatOverflowsTheTally: a header's Size is the
// sender's word, and one near MaxInt64 must not wrap the running sum negative.
func TestUnpackRefusesASizeThatOverflowsTheTally(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)
	writeRegular(writer, "small")
	_ = writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "huge", Mode: 0o600, Size: math.MaxInt64, Format: tar.FormatPAX})

	err := UnpackFetchedTree(&buf, t.TempDir(), UnpackOptions{MaxBytes: 1024})
	if !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("UnpackFetchedTree = %v, want ErrTreeTooLarge", err)
	}
}

// TestFetchLimitsAreSet: a zero here is no limit at all, silently.
func TestFetchLimitsAreSet(t *testing.T) {
	t.Parallel()

	limits := FetchLimits()
	if limits.MaxBytes <= 0 || limits.MaxEntries <= 0 {
		t.Fatalf("FetchLimits = %+v, want both bounded", limits)
	}
}
