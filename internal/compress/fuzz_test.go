package compress

import (
	"bytes"
	"io"
	"testing"
)

// fuzzReadCap bounds what one arbitrary stream may inflate to, so a small input claiming a huge frame is a finding about memory rather than a hang.
const fuzzReadCap = 4 << 20

// FuzzRoundTrip: what comes out of Unpack is byte for byte what went into Pack, compressed or not — the transparency the tar codec's digest contract relies on.
func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte{}, true)
	f.Add([]byte("alpha"), true)
	f.Add(bytes.Repeat([]byte{0}, 1<<16), true)
	f.Add([]byte("plain"), false)

	f.Fuzz(func(t *testing.T, data []byte, compressed bool) {
		var buf bytes.Buffer

		err := Pack(&buf, compressed, func(w io.Writer) error {
			_, err := w.Write(data)

			return err //nolint:wrapcheck // the test's own writer
		})
		if err != nil {
			t.Fatalf("Pack: %v", err)
		}

		var got []byte

		err = Unpack(&buf, compressed, func(r io.Reader) error {
			var readErr error

			got, readErr = io.ReadAll(r)

			return readErr //nolint:wrapcheck // the test's own reader
		})
		if err != nil {
			t.Fatalf("Unpack: %v", err)
		}

		if !bytes.Equal(got, data) {
			t.Fatalf("round trip changed %d bytes into %d", len(data), len(got))
		}
	})
}

// FuzzUnpack reads arbitrary bytes as a zstd stream from a peer: it may refuse them, but it must not panic, hang, or allocate for a frame the bytes only claim.
func FuzzUnpack(f *testing.F) {
	var valid bytes.Buffer

	_ = Pack(&valid, true, func(w io.Writer) error {
		_, err := w.Write([]byte("alpha beta gamma"))

		return err //nolint:wrapcheck // the test's own writer
	})

	f.Add(valid.Bytes())
	f.Add([]byte{})
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd})
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = Unpack(bytes.NewReader(data), true, func(r io.Reader) error {
			_, err := io.Copy(io.Discard, io.LimitReader(r, fuzzReadCap))

			return err //nolint:wrapcheck // the test's own reader
		})
	})
}
