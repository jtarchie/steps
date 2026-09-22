package shim

// A worker keeps what it PRODUCES, not only what it receives.
//
// Before this the cache was written on the receive paths alone, so a placed
// get fetched a tree here, shipped it home, and the next placed step's offer
// for that same tree — on this same machine — was answered "need it", and the
// bytes came straight back. Filing what a fetch packs, under the digest the
// next offer will name, is what makes the round trip stop here (steps#138).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/wire"
)

// fileProduced files each tree a fetch packed under its own digest.
//
// Best-effort in one direction only: a tree that cannot be filed costs the
// next step a transfer, never the fetch that already succeeded, so a missing
// declared output is skipped exactly as PackPaths skips it. What is NOT
// best-effort is the digest — the entry is copied first and hashed from the
// copy, so the name proves the bytes that were filed and not the bytes that
// were meant to be.
//
// ponytail: a copy plus a hash pass, rather than filing off the pack's own
// walk. One tar stream for every output cannot be hashed per artifact, and
// the copy is the same one placing an entry already pays; a reflink where the
// filesystem has one is the upgrade, and it is a measurement away.
func (s *session) fileProduced(fetch wire.Fetch) error {
	if volatileFS(s.fstype) {
		return nil
	}

	trees := map[string]string{}

	switch {
	case len(fetch.Paths) > 0:
		for _, name := range fetch.Paths {
			trees[name] = filepath.Join(s.workdir, name)
		}
	case fetch.Artifact != "":
		trees[fetch.Artifact] = s.workdir
	default:
		return nil
	}

	cache := s.artifactCacheDir()

	for name, src := range trees {
		err := fileTree(cache, src, name)
		if err != nil {
			return err
		}
	}

	return sweepArtifactCache(cache)
}

// volatileFS is a filesystem that is memory: filing a tree there spends RAM
// to save a transfer, and loses it at the next reboot anyway. Received trees
// still land — the step needs them — but a produced one is optional.
func volatileFS(fstype string) bool {
	return fstype == "tmpfs" || fstype == "ramfs"
}

// fileTree copies one tree into the cache under its name, and commits the
// copy under the digest it packs to.
func fileTree(cache, src, name string) error {
	_, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("reading the produced %q: %w", name, err)
	}

	staging, err := stageArtifact(cache)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(staging)
	if err != nil {
		_ = os.RemoveAll(staging)

		return fmt.Errorf("opening the staging directory: %w", err)
	}

	err = copyTree(root, src, name)

	_ = root.Close()

	if err != nil {
		_ = os.RemoveAll(staging)

		return err
	}

	hasher := sha256.New()

	err = wire.PackPaths(hasher, staging, []string{name})
	if err != nil {
		_ = os.RemoveAll(staging)

		return fmt.Errorf("digesting the produced %q: %w", name, err)
	}

	held := filepath.Join(cache, hex.EncodeToString(hasher.Sum(nil)))

	// Already held — by an earlier step, or by the upload that brought this
	// tree in unchanged — is the cache working. What is there is verified on
	// use, so a damaged entry is the next placement's problem and not this
	// one's to second-guess.
	_, err = os.Stat(held)
	if err == nil {
		err = os.RemoveAll(staging)
		if err != nil {
			return fmt.Errorf("discarding a copy the cache already holds: %w", err)
		}

		return nil
	}

	return commitArtifact(staging, held)
}
