package workspace

// Replacing a tree without ever exposing a half-built one: build beside the
// destination, then rename into place.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
)

// stagedMarker sits between a destination's name and a random tag in every
// staging path. Its '~' is refused by config.ValidateArtifactName and its '.'
// by entryStore.path, so a staging directory can never be mistaken for an
// artifact or a cache entry — which matters only after a SIGKILL, the one exit
// that leaves one behind.
const stagedMarker = ".steps~staged-"

// stagingPath is a fresh sibling of dst: same parent, so a rename into dst
// never crosses a filesystem.
// Random rather than fixed, so concurrent writers to one parent — collecting
// matrix cells under max_in_flight — never share one.
func stagingPath(dst string) string {
	var buf [8]byte

	_, _ = rand.Read(buf[:]) // crypto/rand.Read never returns an error

	return dst + stagedMarker + hex.EncodeToString(buf[:])
}

// stageTree has build fill a fresh staging sibling of dst and returns it.
// Whatever a failed build left there is removed with remove, so an
// interrupted copy costs nothing but the work.
func stageTree(dst string, remove func(string) error, build func(staged string) error) (string, error) {
	staged := stagingPath(dst)

	err := build(staged)
	if err != nil {
		discardTree(staged, remove)

		return "", err
	}

	return staged, nil
}

// swapIn installs staged at dst. An existing dst is renamed aside first,
// because rename(2) refuses to replace a non-empty directory, and is put back
// if the install fails — so dst is only ever the old tree or the new one, and
// absent at most for the instant between two renames.
func swapIn(staged, dst string, remove func(string) error) error {
	aside := ""

	_, err := os.Lstat(dst)

	switch {
	case err == nil:
		aside = stagingPath(dst)

		err = os.Rename(dst, aside)
		if err != nil {
			return fmt.Errorf("moving aside %q: %w", dst, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("replacing %q: %w", dst, err)
	}

	err = os.Rename(staged, dst)
	if err != nil {
		if aside != "" {
			restoreErr := os.Rename(aside, dst)
			if restoreErr != nil {
				return fmt.Errorf("installing %q: %w (and restoring the previous tree: %w)", dst, err, restoreErr)
			}
		}

		return fmt.Errorf("installing %q: %w", dst, err)
	}

	if aside != "" {
		discardTree(aside, remove)
	}

	return nil
}

// replaceTree is stageTree then swapIn: dst ends up holding everything build
// produced, or exactly what it held before.
func replaceTree(dst string, remove func(string) error, build func(staged string) error) error {
	staged, err := stageTree(dst, remove, build)
	if err != nil {
		return err
	}

	err = swapIn(staged, dst, remove)
	if err != nil {
		discardTree(staged, remove)

		return err
	}

	return nil
}

// discardTree removes a staging or set-aside tree. Never an error: the tree
// is garbage either way, and the marker in its name keeps it from being read
// as anything else until a prune or a sweep takes it.
func discardTree(dir string, remove func(string) error) {
	err := remove(dir)
	if err != nil {
		slog.Warn("workspace.staging_cleanup_failed", "dir", dir, "error", err)
	}
}
