package workspace

// Artifacts held elsewhere (steps#138, rung 2).
//
// A placed step's outputs stay on the worker that produced them. The build
// records that — the digest and who holds it — and brings the bytes home
// only when something here reads them: a step materializing the artifact as
// an input, a cache that lives on this disk, an in-process reader. Until
// then the orchestrator is one holder among the workers, and not this one.

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/jtarchie/steps/internal/config"
)

// RemoteArtifact is where an artifact's bytes are when they are not here.
type RemoteArtifact struct {
	// Digest names the tree as the holder filed it — the wire digest, which
	// the holder proves before handing it back.
	Digest string
	// Holder is the worker, as the mapping was written, for the error that
	// names it when the tree cannot be brought home.
	Holder string
	// Pull fills dst — an existing, empty directory the build created — with
	// the tree. Injected rather than imported: this package does not know
	// how a worker is dialled, and must not.
	Pull func(ctx context.Context, dst string) error
}

// RemoteHolder is the optional BuildWorkspace capability a placed step uses
// to record an output it left on its worker. Optional because only the
// isolating builds have an artifact store to record against.
type RemoteHolder interface {
	HoldRemote(name string, remote RemoteArtifact) error
}

// HoldRemote implements RemoteHolder: whatever this build held locally under
// name is superseded by what the worker holds. A Capture that follows still
// files the step's local directory — empty, for a deferred output — and the
// first reader replaces it with the pull, so nothing here has to know which
// outputs were kept.
func (b *isolatingBuild) HoldRemote(name string, remote RemoteArtifact) error {
	err := config.ValidateArtifactPath(name)
	if err != nil {
		return fmt.Errorf("remote artifact %q: %w", name, err)
	}

	if remote.Digest == "" || remote.Pull == nil {
		return fmt.Errorf("remote artifact %q: no digest or no way to pull it", name)
	}

	// The local copy — a get's empty resource directory, or an earlier
	// producer's tree — would otherwise be read as the artifact by anything
	// that looked before pulling.
	err = b.backend.remove(filepath.Join(b.artifacts, name))
	if err != nil {
		return fmt.Errorf("remote artifact %q: replacing the local copy: %w", name, err)
	}

	b.remoteMu.Lock()

	if b.remote == nil {
		b.remote = map[string]RemoteArtifact{}
	}

	b.remote[name] = remote
	b.remoteMu.Unlock()

	b.forgetDigests([]string{name})

	return nil
}

func (b *isolatingBuild) remoteArtifact(name string) (RemoteArtifact, bool) {
	b.remoteMu.Lock()
	defer b.remoteMu.Unlock()

	remote, ok := b.remote[name]

	return remote, ok
}

// forgetRemote drops the records of artifacts whose bytes are now here, or
// are gone: a restored cache entry, a reset, a pull that landed.
func (b *isolatingBuild) forgetRemote(names []string) {
	b.remoteMu.Lock()
	defer b.remoteMu.Unlock()

	for _, name := range names {
		delete(b.remote, name)
	}
}

// remoteNames lists every artifact held elsewhere, for a listing that must
// see the whole store.
func (b *isolatingBuild) remoteNames() []string {
	b.remoteMu.Lock()
	defer b.remoteMu.Unlock()

	names := make([]string, 0, len(b.remote))
	for name := range b.remote {
		names = append(names, name)
	}

	return names
}

// ensureLocal brings an artifact home if a worker holds it, so that what
// follows can read it from the artifact store as it always has. A holder
// that no longer has the tree fails by name — the artifact and the machine —
// because running the reader against nothing is the worse outcome.
func (b *isolatingBuild) ensureLocal(ctx context.Context, name string) error {
	remote, ok := b.remoteArtifact(name)
	if !ok {
		return nil
	}

	dst := filepath.Join(b.artifacts, name)

	// A backend-created tree rather than a plain directory, because on
	// btrfs what later steps snapshot must be a subvolume.
	err := b.backend.remove(dst)
	if err != nil {
		return fmt.Errorf("artifact %q: %w", name, err)
	}

	err = b.backend.createEmpty(ctx, dst)
	if err != nil {
		return fmt.Errorf("artifact %q: %w", name, err)
	}

	err = remote.Pull(ctx, dst)
	if err != nil {
		_ = b.backend.remove(dst)

		return fmt.Errorf("artifact %q is held by worker %s and could not be brought home: %w", name, remote.Holder, err)
	}

	b.forgetRemote([]string{name})
	b.forgetDigests([]string{name})

	return nil
}
