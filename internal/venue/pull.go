package venue

// Bringing a tree home from the worker that holds it (steps#138, rung 2).
//
// A deferred fetch leaves a step's outputs on the worker, filed under their
// digests. This is the other half: a fresh session to that worker, one
// FrameGet, and the tree lands where the caller says — the same shape as a
// fetch-all coming home, and verified by the worker before it is sent.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

// Pull fills dst — an existing, empty directory — with the tree the worker
// spec names holds under digest, filed as name. It reports the bytes that
// crossed. A worker that no longer holds the tree is an error naming it; the
// caller decides what a lost holder costs.
func Pull(ctx context.Context, spec shell.RunnerSpec, name, digest, dst string) (int64, error) {
	worker, err := ParseWorker(spec.Worker)
	if err != nil {
		return 0, err
	}

	//nolint:contextcheck // opening the artifact store reads only local config
	blobs, err := artifactStoreFor(spec.ArtifactStore)
	if err != nil {
		return 0, err
	}

	// No tree of its own, and never redialled: a pull that lost its worker
	// mid-stream has nothing to re-send, and the caller retries or fails.
	s := &session{worker: worker, blobs: blobs, tag: spec.WorkerTag, noRedial: true}

	//nolint:contextcheck // close runs under its own bound, deliberately not the caller's context
	defer func() { _ = s.close() }()

	err = s.ensure(ctx)
	if err != nil {
		return 0, err
	}

	stop := s.watchTransfer(ctx)
	defer stop()

	err = s.pullInto(name, digest, dst)
	if err != nil {
		return 0, fmt.Errorf("worker %q: %w", spec.Worker, err)
	}

	return s.receivedArtifactBytes.Load(), nil
}

// Push asks the worker spec names to put the tree it holds under digest, filed
// as name, at url — a presigned PUT the caller minted. The bytes go from
// that worker to the store and never through this machine.
func Push(ctx context.Context, spec shell.RunnerSpec, name, digest, url string) error {
	worker, err := ParseWorker(spec.Worker)
	if err != nil {
		return err
	}

	//nolint:contextcheck // opening the artifact store reads only local config
	blobs, err := artifactStoreFor(spec.ArtifactStore)
	if err != nil {
		return err
	}

	s := &session{worker: worker, blobs: blobs, tag: spec.WorkerTag, noRedial: true}

	//nolint:contextcheck // close runs under its own bound, deliberately not the caller's context
	defer func() { _ = s.close() }()

	err = s.ensure(ctx)
	if err != nil {
		return err
	}

	stop := s.watchTransfer(ctx)
	defer stop()

	op := s.nextOp()

	err = s.write(wire.Frame{Type: wire.FramePush, Op: op}, wire.Push{Name: name, Digest: digest, URL: url})
	if err != nil {
		return err
	}

	err = s.awaitEnd(op, "confirming the artifact reached the store")
	if err != nil {
		return fmt.Errorf("worker %q: %w", spec.Worker, err)
	}

	return nil
}

// pullInto asks for one held tree and lands it in dst, an existing empty
// directory.
func (s *session) pullInto(name, digest, dst string) error {
	// Beside dst, so the swap below is a rename on one filesystem.
	staging, err := os.MkdirTemp(filepath.Dir(dst), ".steps-pull-")
	if err != nil {
		return fmt.Errorf("staging the pulled tree: %w", err)
	}

	defer func() { _ = os.RemoveAll(staging) }()

	op := s.nextOp()

	err = s.write(wire.Frame{Type: wire.FrameGet, Op: op}, wire.Get{Name: name, Digest: digest})
	if err != nil {
		return err
	}

	err = s.receive(op, staging)
	if err != nil {
		return err
	}

	from := filepath.Join(staging, name)

	err = adoptFetchedDir(from, dst, name)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(from)
	if err != nil {
		return fmt.Errorf("reading the pulled tree: %w", err)
	}

	for _, entry := range entries {
		err = os.Rename(filepath.Join(from, entry.Name()), filepath.Join(dst, entry.Name()))
		if err != nil {
			return fmt.Errorf("placing the pulled %q: %w", entry.Name(), err)
		}
	}

	return nil
}
