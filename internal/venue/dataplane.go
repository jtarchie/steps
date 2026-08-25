package venue

// The URL data plane, orchestrator side: trees move through the artifact
// store and the tunnel carries control frames only — the design #81's SSM
// tunnels depend on, since those carry single-digit MB/s while S3 carries
// whatever the worker's NIC does.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// wireTTL bounds each presigned URL. Minted fresh per transfer, so the bound
// is per operation, not per session — a 24-hour SSM session never holds a
// URL this old.
const wireTTL = 15 * time.Minute

// uploadViaStore ships the step's tree as one blob: packed and hashed
// locally, uploaded unless the store already holds it, and named to the shim
// by a presigned GET.
//
// The key is the hash of the TAR stream (reproducible by the codec's own
// test), so a redial after a worker died — or a spot eviction's replacement
// venue — finds the tree already uploaded and skips straight to the URL.
// These objects are transient by design: a lifecycle rule expiring wire/ is
// safe, since the worst case is the next session re-uploading.
func (s *session) uploadViaStore(ctx context.Context) error {
	key, staged, err := s.packTreeToFile()
	if staged != "" {
		defer func() { _ = os.Remove(staged) }()
	}

	if err != nil {
		return err
	}

	has, err := s.blobs.Has(ctx, key)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if !has {
		err = s.blobs.PutFile(ctx, key, staged)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
	}

	url, err := s.blobs.PresignGet(ctx, key, wireTTL)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	op := s.nextOp()

	err = s.write(wire.Frame{Type: wire.FrameUpload, Op: op}, wire.Upload{URL: url})
	if err != nil {
		return err
	}

	// The same acknowledgement the tunnel waits for, for the same reason: a
	// command must never run against a tree the far end did not confirm.
	return s.awaitEnd(op, "acknowledging the tree")
}

// packTreeToFile packs the tree as a store blob — zstd over tar — into a
// temporary file, returning the content key and the file's path. The path
// comes back even on error so the caller can always clean up.
func (s *session) packTreeToFile() (key, name string, err error) {
	staged, err := os.CreateTemp("", "steps-wire-*")
	if err != nil {
		return "", "", fmt.Errorf("staging the step tree: %w", err)
	}

	defer func() { _ = staged.Close() }()

	// The hash is of the tar bytes, before compression: that is the stream
	// the codec's reproducibility test pins, so the same tree keys the same
	// way across sessions and processes.
	hasher := sha256.New()

	err = compress.Pack(staged, true, func(w io.Writer) error {
		return wire.PackTree(io.MultiWriter(w, hasher), s.cwd)
	})
	if err != nil {
		return "", staged.Name(), fmt.Errorf("packing the step tree: %w", err)
	}

	return "wire/" + hex.EncodeToString(hasher.Sum(nil)), staged.Name(), nil
}

// fetchViaStore brings the declared outputs back through the store: the shim
// PUTs to a URL minted for this one fetch, and this end reads the object,
// stages, and swaps exactly as the tunnel path does.
func (s *session) fetchViaStore(ctx context.Context) error {
	key, err := fetchKey()
	if err != nil {
		return err
	}

	url, err := s.blobs.PresignPut(ctx, key, wireTTL)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	op := s.nextOp()

	err = s.write(wire.Frame{Type: wire.FrameFetch, Op: op}, wire.Fetch{Paths: s.outputs, URL: url})
	if err != nil {
		return err
	}

	err = s.awaitEnd(op, "confirming the outputs were shipped")
	if err != nil {
		return err
	}

	body, err := s.blobs.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() { _ = body.Close() }()

	// Staged then swapped, same shape and same reasons as the tunnel fetch.
	staging, err := os.MkdirTemp(s.cwd, ".steps-fetch-")
	if err != nil {
		return fmt.Errorf("staging the fetched outputs: %w", err)
	}

	defer func() { _ = os.RemoveAll(staging) }()

	err = compress.Unpack(body, true, func(r io.Reader) error {
		return wire.UnpackTree(r, staging)
	})
	if err != nil {
		return fmt.Errorf("unpacking what the worker shipped: %w", err)
	}

	err = s.swapFetched(staging)
	if err != nil {
		return err
	}

	// Best-effort: a fetch object is one-shot, and the lifecycle rule is the
	// backstop for the ones a crash leaves behind.
	_ = s.blobs.Delete(ctx, key)

	return nil
}

// fetchKey names one fetch's transient object. Random rather than
// content-keyed, because a PUT URL has to be minted before the content
// exists.
func fetchKey() (string, error) {
	suffix := make([]byte, 16)

	_, err := rand.Read(suffix)
	if err != nil {
		return "", fmt.Errorf("naming the fetch object: %w", err)
	}

	return "wire/out-" + hex.EncodeToString(suffix), nil
}

// awaitEnd reads one frame and requires it to be this operation's FrameEnd.
func (s *session) awaitEnd(op uint32, what string) error {
	frame, err := s.awaitOperationFrame()
	if err != nil {
		return err
	}

	if frame.Type != wire.FrameEnd || frame.Op != op {
		return fmt.Errorf("%w: the worker answered a type %d frame for operation %d instead of %s",
			wire.ErrProtocol, frame.Type, frame.Op, what)
	}

	return nil
}
