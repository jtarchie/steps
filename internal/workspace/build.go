package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/jtarchie/steps/internal/config"
)

type isolatingBuild struct {
	backend     treeBackend
	root        string
	artifacts   string
	stepsDir    string
	keep        bool
	cache       *resourceCache
	stepCache   *stepCache
	taskCaches  *entryStore
	stepCounter atomic.Int64

	// digests memoizes artifactDigest per artifact name. An artifact's bytes
	// change only when something in this file replaces the directory (a get
	// fetching, a step capturing, a matrix resetting, a cache hit restoring),
	// and each of those forgets the entry — so the memo is exact rather than
	// merely close, and hashing a large checkout costs the same whether one
	// step reads it or ten do.
	digestMu    sync.Mutex
	digests     map[string]string
	generations map[string]uint64

	// remote is every artifact a worker holds on this build's behalf, by
	// name — see remote.go.
	remoteMu sync.Mutex
	remote   map[string]RemoteArtifact
	pulling  map[string]chan struct{}
}

// artifactDigest is the content hash of one artifact in the build store, for
// the step cache's action key.
//
// An artifact that does not exist digests as the empty tree rather than
// failing: a step may legitimately declare an input no earlier step produced
// (materializeSpace would fail later, and with a better message), and a cache
// lookup is not the place to decide that.
func (b *isolatingBuild) artifactDigest(name string) (string, error) {
	digest, generation, ok := b.rememberedDigest(name)
	if ok {
		return digest, nil
	}

	// A tree a worker holds is identified by the digest it is filed under rather than pulled to be walked: the worker names it with the same treedigest, so the key is the one a local copy would have.
	if remote, held := b.remoteArtifact(name); held {
		return remote.Digest, nil
	}

	// Outside the lock: walking a large checkout is the expensive part, and
	// holding a build-wide mutex across it would serialize every concurrent
	// branch behind one tree walk. Two branches racing the same artifact each
	// compute it and agree, which costs one redundant walk and no correctness.
	digest, err := digestTree(filepath.Join(b.artifacts, name))
	if err != nil {
		// A step may legitimately declare an input no earlier step produced;
		// materializeSpace fails on that later, with a better message than a
		// cache lookup could give.
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}

		digest = ""
	}

	b.rememberDigest(name, digest, generation)

	return digest, nil
}

// rememberedDigest reports a memoized digest, and the generation the artifact
// was at when it was read — what tells rememberDigest whether the bytes moved
// while it was being walked.
func (b *isolatingBuild) rememberedDigest(name string) (digest string, generation uint64, ok bool) {
	b.digestMu.Lock()
	defer b.digestMu.Unlock()

	digest, ok = b.digests[name]

	return digest, b.generations[name], ok
}

// rememberDigest memoizes a freshly computed digest, unless the artifact was
// replaced while it was being computed — in which case the digest describes
// bytes that are already gone, and memoizing it would hand the next step a key
// for content nobody can read any more.
func (b *isolatingBuild) rememberDigest(name, digest string, generation uint64) {
	b.digestMu.Lock()
	defer b.digestMu.Unlock()

	if b.generations[name] != generation {
		return
	}

	if b.digests == nil {
		b.digests = map[string]string{}
	}

	b.digests[name] = digest
}

// superseded is what a capture or a fresh fetch says about an artifact: the
// bytes are HERE now, whatever a worker still holds and whatever digest was
// remembered. Both records or neither — a stale hold pulls the old tree over
// a new capture on the next read, which is data loss with nothing red.
func (b *isolatingBuild) superseded(names []string) {
	b.forgetRemote(names)
	b.forgetDigests(names)
}

// forgetDigests drops memoized digests for artifacts whose bytes just changed,
// and bumps their generation so a walk already in flight over the old content
// cannot install its answer afterwards.
func (b *isolatingBuild) forgetDigests(names []string) {
	b.digestMu.Lock()
	defer b.digestMu.Unlock()

	if b.generations == nil {
		b.generations = map[string]uint64{}
	}

	for _, name := range names {
		delete(b.digests, name)
		b.generations[name]++
	}
}

func (b *isolatingBuild) FetchResource(ctx context.Context, name, cacheKey string, fetch func(dir string) error) (string, error) {
	dir, err := b.ResourceDir(ctx, name)
	if err != nil {
		return "", err
	}

	// After the fetch on BOTH paths, not only the cached one: ResourceDir's own
	// forget happens before these bytes exist, so without this the digest
	// memoized for an artifact could be the empty tree it was created as.
	defer b.forgetDigests([]string{name})

	if b.cache == nil || cacheKey == "" {
		return dir, fetch(dir)
	}

	// The cache lives on this disk, so it is a local reader: a tree a placed
	// in: left on its worker comes home to be filed.
	return dir, b.cache.Fetch(ctx, cacheKey, dir, func() error {
		err := fetch(dir)
		if err != nil {
			return err
		}

		return b.ensureLocal(ctx, name)
	})
}

func (b *isolatingBuild) ResourceDir(ctx context.Context, name string) (string, error) {
	err := config.ValidateArtifactName(name)
	if err != nil {
		return "", fmt.Errorf("resource %q: %w", name, err)
	}

	dir := filepath.Join(b.artifacts, name)

	err = b.backend.createEmpty(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("could not create resource dir %q: %w", dir, err)
	}

	b.superseded([]string{name})

	return dir, nil
}

// ResetArtifact is what a collecting across: block uses (see
// internal/pipeline): it replaces whatever the store holds under name with a
// fresh empty directory, before any cell captures. That is what (a) restores the "a later producer replaces an
// earlier one's artifact" sequential contract that per-coordinate capture
// alone cannot — each cell's Capture replaces only its own subdirectory — and
// (b) guarantees the collected artifact EXISTS even when every cell failed or
// the matrix expanded to zero cells, so the consumer walks what survived
// (possibly nothing) instead of failing to materialize a missing input.
func (b *isolatingBuild) ResetArtifact(ctx context.Context, name string) error {
	err := config.ValidateArtifactName(name)
	if err != nil {
		return fmt.Errorf("reset artifact: %w", err)
	}

	dst := filepath.Join(b.artifacts, name)

	err = b.backend.remove(dst)
	if err != nil {
		return fmt.Errorf("resetting artifact %q: %w", name, err)
	}

	err = b.backend.createEmpty(ctx, dst)
	if err != nil {
		return fmt.Errorf("resetting artifact %q: %w", name, err)
	}

	b.forgetRemote([]string{name})
	b.forgetDigests([]string{name})

	return nil
}

// Close removes the build's root directory and the materialized trees under
// it, through backend.remove.
func (b *isolatingBuild) Close() error {
	// The provider prints its own kept-root line when it owns the root; a
	// build under a user-supplied workspace.root: has none, so say it here.
	if b.keep {
		slog.Debug("workspace.kept", "dir", b.root)

		return nil
	}

	err := b.backend.remove(b.root)
	if err != nil {
		return fmt.Errorf("could not remove build workspace %q: %w", b.root, err)
	}

	return nil
}
