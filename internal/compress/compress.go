// Package compress is the one opinion about zstd this repo holds.
//
// internal/wire stays stdlib-only so the framed protocol cannot drift by
// acquiring a dependency, and there is no zstd in the standard library — so
// the compressing wrap lives one package over, shared by every path that
// ships a tar stream: the venue's upload, the shim's fetch, and any blob a
// content-addressed store holds. Compression here is a transparent stream
// wrapper: what comes out of a Reader is byte-for-byte what went into a
// Writer, so the digest contract stays owned by the tar codec alone.
package compress

import (
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// NewWriter wraps w in a zstd encoder. Close it to finish the stream — an
// unclosed encoder is a truncated frame the far end cannot decode.
//
// Concurrency 1, deliberately: encoding happens on the calling goroutine, so
// an abandoned encoder on an error path leaks nothing for goleak to find.
func NewWriter(w io.Writer) (io.WriteCloser, error) {
	encoder, err := zstd.NewWriter(w, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("opening a zstd stream: %w", err)
	}

	return encoder, nil
}

// NewReader wraps r in a zstd decoder. Close releases it; concurrency 1 for
// the same reason as NewWriter.
func NewReader(r io.Reader) (io.ReadCloser, error) {
	decoder, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("reading a zstd stream: %w", err)
	}

	return decoder.IOReadCloser(), nil
}
