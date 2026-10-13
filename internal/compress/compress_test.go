package compress

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestPackReportsTheWritersFailure: a tree that failed to pack partway must not be reported as packed — the caller would upload the truncated stream under the tree's digest.
func TestPackReportsTheWritersFailure(t *testing.T) {
	broken := errors.New("tree vanished mid-walk")

	err := Pack(io.Discard, true, func(io.Writer) error { return broken })
	if !errors.Is(err, broken) {
		t.Fatalf("Pack = %v, want the writer's own error", err)
	}
}

// TestUnpackRefusesAWindowPastTheLimit: a stream's header names the window the
// decoder must allocate, and the library's own ceiling is 512MB. A frame
// asking for 256MB is refused from its header alone, before any memory goes.
func TestUnpackRefusesAWindowPastTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame []byte
		want  error
	}{
		// A window descriptor of exponent 18: 1<<(10+18) = 256MB.
		{"a declared window", []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 18 << 3}, zstd.ErrWindowSizeExceeded},
		// Single segment with an 8-byte content size of 256MB: the window IS
		// the content size, and no window descriptor is checked.
		{"a single segment's content size", []byte{0x28, 0xb5, 0x2f, 0xfd, 0xe0, 0, 0, 0, 0x10, 0, 0, 0, 0}, zstd.ErrDecoderSizeExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Unpack(bytes.NewReader(tc.frame), true, func(r io.Reader) error {
				_, err := io.Copy(io.Discard, r)

				return err //nolint:wrapcheck // the test reads the decoder's own error
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Unpack = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestUnpackDecodesPastTheMemoryLimit: WithDecoderMaxMemory caps the total
// output of an in-memory decode, so a stream must still decode to more than
// it — a tree's size is wire's to bound, not the decoder's.
func TestUnpackDecodesPastTheMemoryLimit(t *testing.T) {
	const size = maxWindow + 1<<20

	var buf bytes.Buffer

	err := Pack(&buf, true, func(w io.Writer) error {
		_, err := io.CopyN(w, zeros{}, size)

		return err //nolint:wrapcheck // the test reads the encoder's own error
	})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	var got int64

	err = Unpack(&buf, true, func(r io.Reader) error {
		var err error

		got, err = io.Copy(io.Discard, r)

		return err //nolint:wrapcheck // the test reads the decoder's own error
	})
	if err != nil || got != size {
		t.Fatalf("Unpack = %v after %d bytes, want %d", err, got, size)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

// TestUnpackAcceptsTheLargestWindowPackWrites: the limit must not refuse a
// stream this package itself produced at its best compression.
func TestUnpackAcceptsTheLargestWindowPackWrites(t *testing.T) {
	var buf bytes.Buffer

	encoder, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	payload := bytes.Repeat([]byte("steps"), 1<<20)

	_, _ = encoder.Write(payload)
	_ = encoder.Close()

	var got bytes.Buffer

	err = Unpack(&buf, true, func(r io.Reader) error {
		_, err := io.Copy(&got, r)

		return err //nolint:wrapcheck // the test reads the decoder's own error
	})
	if err != nil || !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("Unpack = %v (%d bytes), want the payload back", err, got.Len())
	}
}
