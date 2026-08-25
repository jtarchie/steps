package venue

// A worker whose shim never learned compression.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/shim"
	"github.com/jtarchie/steps/internal/wire"
)

// legacyShimEnv makes the helper-process shim behave like one built before
// compression and the URL data plane existed: the proposals in the hello are
// invisible to it, so it answers without the fields, speaks raw tar, and
// takes its trees over the tunnel.
const legacyShimEnv = "STEPS_TEST_LEGACY_SHIM"

// serveLegacyShim runs the REAL shim behind a pump that strips the
// negotiation proposals from the hello — which is exactly what an older
// binary's json decoder does with fields it never learned. Everything after
// the hello is the real shim, so this pins the whole floor path, not a
// stub's opinion of it.
func serveLegacyShim() {
	reader, writer := io.Pipe()

	go func() {
		decoder := wire.NewDecoder(os.Stdin)
		encoder := wire.NewEncoder(writer)

		for {
			frame, err := decoder.Read()
			if err != nil {
				_ = writer.CloseWithError(err)

				return
			}

			if frame.Type == wire.FrameHello {
				var hello wire.Hello

				err = wire.DecodeJSON(frame, &hello)
				if err != nil {
					_ = writer.CloseWithError(err)

					return
				}

				hello.Compression = ""
				hello.DataPlane = ""

				err = encoder.WriteJSON(wire.FrameHello, frame.Op, hello)
				if err != nil {
					return
				}

				continue
			}

			err = encoder.Write(frame)
			if err != nil {
				return
			}
		}
	}()

	build, err := shim.SelfBuild()
	if err != nil {
		os.Exit(1)
	}

	err = shim.Serve(context.Background(), reader, os.Stdout, shim.Options{Build: build})
	if err != nil {
		os.Exit(1)
	}

	os.Exit(0)
}

// TestVenueFallsBackToRawWithALegacyShim is the degradation half of the
// compression negotiation: a shim that answers the hello without the field is
// an older binary, and the venue must speak raw tar to it rather than ship a
// stream it cannot decode. Raw is the floor both ends always share.
func TestVenueFallsBackToRawWithALegacyShim(t *testing.T) {
	t.Setenv(legacyShimEnv, "1")

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("Run against a legacy shim: %v", err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if got != "seed\n" {
		t.Errorf("out/report.txt = %q, want %q", got, "seed\n")
	}
}
