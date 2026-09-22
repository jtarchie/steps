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
	"sort"
	"sync/atomic"
	"time"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/shell"
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
	names, err := treeArtifacts(s.cwd)
	if err != nil {
		return err
	}

	artifacts := make([]wire.UploadArtifact, 0, len(names)+len(s.remoteInputs))

	for _, name := range names {
		artifact, uploadErr := s.uploadArtifact(ctx, name)
		if uploadErr != nil {
			return uploadErr
		}

		artifacts = append(artifacts, artifact)
	}

	for _, name := range sortedNames(s.remoteInputs) {
		artifact, offerErr := s.offerRemoteArtifact(ctx, name, s.remoteInputs[name])
		if offerErr != nil {
			return offerErr
		}

		artifacts = append(artifacts, artifact)
	}

	op := s.nextOp()

	err = s.write(wire.Frame{Type: wire.FrameUpload, Op: op}, wire.Upload{Artifacts: artifacts})
	if err != nil {
		return err
	}

	// The same acknowledgement the tunnel waits for, for the same reason: a
	// command must never run against a tree the far end did not confirm.
	return s.awaitEnd(op, "acknowledging the tree")
}

// treeArtifacts names the top-level entries of a step's tree.
//
// Top-level, because that is the grain internal/workspace built the tree at:
// each entry is one declared input or output, and it is the unit two steps
// can share. Finer would key on files a step might legitimately change;
// coarser is the whole tree, which never repeats — two steps of one job
// declare different outputs, so their trees differ by an empty directory and
// hash differently.
func treeArtifacts(cwd string) ([]string, error) {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return nil, fmt.Errorf("reading the step tree: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names, nil
}

// uploadArtifact puts one entry in the store if it is not already there, and
// says where to get it.
func (s *session) uploadArtifact(ctx context.Context, name string) (wire.UploadArtifact, error) {
	// Always zstd on this plane: the blob is stored and fetched by this
	// code at both ends, so nothing else has an opinion about its encoding.
	digest, staged, err := s.packArtifactToFile(name, true)
	if staged != "" {
		defer func() { _ = os.Remove(staged) }()
	}

	if err != nil {
		return wire.UploadArtifact{}, err
	}

	key := "wire/" + digest

	has, err := s.blobs.Has(ctx, key)
	if err != nil {
		return wire.UploadArtifact{}, fmt.Errorf("%w", err)
	}

	if !has {
		err = s.blobs.PutFile(ctx, key, staged)
		if err != nil {
			return wire.UploadArtifact{}, fmt.Errorf("%w", err)
		}

		// Counted here and only here, so a blob the store already held is not
		// billed to a session that did not push it — which is the same thing
		// the tunnel's counter means, and the reason a placement can say
		// whether a worker was cold.
		//
		// ponytail: what the WORKER pulled is still uncounted, so a step whose
		// blobs were all already in the store reads 0 B even against a cold
		// machine. Honestly fixing that is a protocol change — the shim
		// reporting the bytes it fetched in its acknowledgement — not another
		// guess made at this end.
		s.sentArtifactBytes.Add(stagedSize(staged))
	}

	url, err := s.blobs.PresignGet(ctx, key, wireTTL)
	if err != nil {
		return wire.UploadArtifact{}, fmt.Errorf("%w", err)
	}

	return wire.UploadArtifact{Name: name, Digest: digest, URL: url}, nil
}

// offerRemoteArtifact names an input another worker holds: in the store
// already, or pushed there by its holder on request. Nothing is read or
// written on this machine, which is the whole point — the orchestrator mints
// the URLs and knows who holds what, as Concourse's web node does.
func (s *session) offerRemoteArtifact(ctx context.Context, name string, input shell.RemoteInput) (wire.UploadArtifact, error) {
	key := "wire/" + input.Digest

	has, err := s.blobs.Has(ctx, key)
	if err != nil {
		return wire.UploadArtifact{}, fmt.Errorf("%w", err)
	}

	if !has {
		put, presignErr := s.blobs.PresignPut(ctx, key, wireTTL)
		if presignErr != nil {
			return wire.UploadArtifact{}, fmt.Errorf("%w", presignErr)
		}

		err = Push(ctx, shell.RunnerSpec{Worker: input.Holder, ArtifactStore: s.worker.ArtifactStore}, name, input.Digest, put)
		if err != nil {
			return wire.UploadArtifact{}, fmt.Errorf("input %q is held by worker %s and could not reach the store: %w", name, input.Holder, err)
		}
	}

	url, err := s.blobs.PresignGet(ctx, key, wireTTL)
	if err != nil {
		return wire.UploadArtifact{}, fmt.Errorf("%w", err)
	}

	return wire.UploadArtifact{Name: name, Digest: input.Digest, URL: url}, nil
}

func sortedNames(inputs map[string]shell.RemoteInput) []string {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// stagedSize is how big the blob this end just pushed was, or zero if the
// staged file cannot be measured. Zero rather than an error: a byte count is
// a report, and failing an upload that SUCCEEDED because its accounting did
// not would be the worse answer.
func stagedSize(staged string) int64 {
	info, err := os.Stat(staged)
	if err != nil {
		return 0
	}

	return info.Size()
}

// packArtifactToFile stages one entry and returns the digest of its tar
// bytes.
//
// Hashed BEFORE compression, which is the stream the codec's reproducibility
// test pins — so the same artifact keys the same way across sessions,
// processes and workers, and identically whether it travelled compressed or
// raw. A digest that moved with the encoding would give one worker two names
// for the same bytes.
func (s *session) packArtifactToFile(name string, zstd bool) (digest, staged string, err error) {
	file, err := os.CreateTemp("", "steps-wire-*")
	if err != nil {
		return "", "", fmt.Errorf("staging an artifact: %w", err)
	}

	hasher := sha256.New()

	err = compress.Pack(file, zstd, func(w io.Writer) error {
		return wire.PackPaths(io.MultiWriter(w, hasher), s.cwd, []string{name})
	})

	// Checked, not dropped, because the DIGEST is taken off the tar stream and
	// not off this file: a delayed-allocation or network filesystem reports
	// ENOSPC only here, and a short file published under the full tree's
	// content key is a poisoning nothing downstream can detect — both caches
	// are content-addressed and neither re-reads what it holds.
	closeErr := file.Close()

	if err != nil {
		return "", file.Name(), fmt.Errorf("packing artifact %q: %w", name, err)
	}

	if closeErr != nil {
		return "", file.Name(), fmt.Errorf("staging artifact %q: %w", name, closeErr)
	}

	return hex.EncodeToString(hasher.Sum(nil)), file.Name(), nil
}

// fetchViaStore brings the declared outputs back through the store: the shim
// PUTs to a URL minted for this one fetch, and this end reads the object,
// stages, and swaps exactly as the tunnel path does.
func (s *session) fetchViaStore(ctx context.Context, paths []string, artifact string) error {
	key, err := fetchKey()
	if err != nil {
		return err
	}

	url, err := s.blobs.PresignPut(ctx, key, wireTTL)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	op := s.nextOp()

	err = s.write(wire.Frame{Type: wire.FrameFetch, Op: op}, wire.Fetch{Paths: paths, URL: url, Artifact: artifact})
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

	err = compress.Unpack(&tallyReader{r: body, n: &s.receivedArtifactBytes}, true, func(r io.Reader) error {
		return wire.UnpackFetchedTree(r, staging)
	})
	if err != nil {
		return fmt.Errorf("unpacking what the worker shipped: %w", err)
	}

	err = s.swapFetched(staging, paths, artifact)
	if err != nil {
		return err
	}

	// Best-effort: a fetch object is one-shot, and the lifecycle rule is the
	// backstop for the ones a crash leaves behind.
	_ = s.blobs.Delete(ctx, key)

	return nil
}

// tallyReader is countingWriter for the store plane, where the outputs
// arrive as a response body rather than as frames.
type tallyReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *tallyReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))

	return n, err //nolint:wrapcheck // a pass-through; the caller owns the context
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
		// Marked, not merely reported: an answer for the wrong operation means
		// the conversation lost its place, so the frames this one did not
		// consume are still queued for whatever reads next. The run loop and
		// the tunnel pump already say so; the store plane said nothing and was
		// reused.
		return s.desync("the worker answered a type %d frame for operation %d instead of %s",
			frame.Type, frame.Op, what)
	}

	return nil
}
