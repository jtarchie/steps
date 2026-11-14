package venue

// Bringing home only what a step changed in an input it also declared as an output (steps#246 S6).

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/treedigest"
	"github.com/jtarchie/steps/internal/wire"
)

// listingScript prints, for every directory in the upper layer, its path (D) and the names its merged view holds (E), NUL-separated. Any change in the overlay copies up its parent directory, so these listings name every deletion: a host entry under one of them that the merged view lacks is gone.
const listingScript = `cd /u && find . -type d -exec sh -c 'for d; do [ -d "/m/$d" ] || continue; printf "D%s\0" "$d"; for e in "/m/$d"/* "/m/$d"/.[!.]* "/m/$d"/..?*; do if [ -e "$e" ] || [ -L "$e" ]; then printf "E%s\0" "${e##*/}"; fi; done; done' sh {} +`

// fetchChanges brings an input-as-output home as its overlay's upper layer: the files the step wrote, then the deletions its directory listings show, instead of the whole merged tree.
func (s *plusSession) fetchChanges(ctx context.Context, name, overlay, upper string) error {
	listing, err := s.mergedListings(ctx, overlay, upper)
	if err != nil {
		return fmt.Errorf("fetching %s's changes: %w", name, err)
	}

	staging, err := os.MkdirTemp(s.spec.Cwd, ".steps-fetch-")
	if err != nil {
		return fmt.Errorf("fetching %s's changes: %w", name, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	err = s.fetchUpper(ctx, upper, staging)
	if err != nil {
		return fmt.Errorf("fetching %s's changes: %w", name, err)
	}

	// Everything has arrived before anything at home is touched, so a failed transfer leaves the last fetch's tree.
	err = applyChanges(staging, filepath.Join(s.spec.Cwd, name), listing)
	if err != nil {
		return fmt.Errorf("fetching %s's changes: %w", name, err)
	}

	return nil
}

// applyChanges prunes home by the listings, then writes staging over it, both confined by os.Root.
func applyChanges(staging, dst string, listing map[string]map[string]bool) error {
	stage, err := os.OpenRoot(staging)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = stage.Close() }()

	home, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = home.Close() }()

	err = prune(home, listing)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return overlayOnto(stage, home)
}

// mergedListings runs listingScript over the merged view and the upper layer, both read-only.
func (s *plusSession) mergedListings(ctx context.Context, overlay, upper string) (map[string]map[string]bool, error) {
	code, stdout, stderr, err := s.docker.RunOnce(ctx, dockerapi.ContainerSpec{
		Image:   treedigest.Image,
		Cmd:     []string{"sh", "-c", listingScript},
		Labels:  shell.OwnershipLabels(),
		Mounts:  []dockerapi.Bind{volumeMount(overlay, "/m", true), volumeMount(upper, "/u", true)},
		Network: "none",
	})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	if code != 0 {
		return nil, fmt.Errorf("%w: exit %d: %s", errListingFailed, code, stderr)
	}

	s.received.Add(int64(len(stdout)))

	return parseListings(stdout)
}

// errListingFailed is the listing container saying no.
var errListingFailed = errors.New("listing the changed directories failed")

// errBadListing is a listing naming a path outside the tree.
var errBadListing = errors.New("the changed-directory listing names a path outside the tree")

// parseListings reads listingScript's records into directory -> the names it holds; every name is checked to stay inside the tree, since it decides what is removed at home.
func parseListings(out string) (map[string]map[string]bool, error) {
	dirs := map[string]map[string]bool{}

	var current map[string]bool

	for record := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		if record == "" {
			continue
		}

		next, err := readRecord(dirs, current, record)
		if err != nil {
			return nil, err
		}

		current = next
	}

	return dirs, nil
}

// readRecord files one D or E record, returning the directory E records now belong to.
func readRecord(dirs map[string]map[string]bool, current map[string]bool, record string) (map[string]bool, error) {
	value := record[1:]

	switch record[0] {
	case 'D':
		dir := path.Clean(value)
		if !filepath.IsLocal(dir) && dir != "." {
			return nil, fmt.Errorf("%w: %q", errBadListing, value)
		}

		dirs[dir] = map[string]bool{}

		return dirs[dir], nil
	case 'E':
		if current == nil || !isEntryName(value) {
			return nil, fmt.Errorf("%w: %q", errBadListing, value)
		}

		current[value] = true

		return current, nil
	default:
		return nil, fmt.Errorf("%w: %q", errBadListing, record)
	}
}

// isEntryName is one path element naming something in a directory.
func isEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "/")
}

// fetchUpper copies the upper layer into staging, minus overlay's whiteouts (character devices), which stand for deletions the listings already carry.
func (s *plusSession) fetchUpper(ctx context.Context, upper, staging string) error {
	holder, err := s.docker.CreateHolder(ctx, "steps-upper-read-"+randomSuffix(), treedigest.Image, shell.OwnershipLabels(), []dockerapi.Bind{volumeMount(upper, holderMount, true)})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		_ = s.docker.RemoveContainer(removeCtx, holder)
	}()

	content, err := s.docker.GetArchive(ctx, holder, holderMount+"/.")
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = content.Close() }()

	reader, writer := io.Pipe()

	go func() { writer.CloseWithError(dropWhiteouts(&byteCounter{r: content, n: &s.received}, writer)) }()

	err = wire.UnpackFetchedTree(reader, staging, fetchLimits())
	_ = reader.CloseWithError(err)

	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// dropWhiteouts copies a tar stream, leaving out character devices: an overlay records a deleted file as one, and no step output is ever one.
func dropWhiteouts(from io.Reader, to io.Writer) error {
	in, out := tar.NewReader(from), tar.NewWriter(to)

	for {
		header, err := in.Next()
		if errors.Is(err, io.EOF) {
			return out.Close() //nolint:wrapcheck // the unpack names the tree
		}

		if err != nil {
			return err //nolint:wrapcheck // as above
		}

		if header.Typeflag == tar.TypeChar {
			continue
		}

		err = out.WriteHeader(header)
		if err != nil {
			return err //nolint:wrapcheck // as above
		}

		_, err = io.Copy(out, in) //nolint:gosec // bounded by the unpack's own limits
		if err != nil {
			return err //nolint:wrapcheck // as above
		}
	}
}

// prune removes, under each listed directory of home, what the merged view no longer holds. Parents before children, through os.Root: a directory that is a symlink at home is removed rather than followed, so nothing outside the output is ever listed or removed.
func prune(home *os.Root, listing map[string]map[string]bool) error {
	for _, dir := range slices.Sorted(maps.Keys(listing)) {
		info, err := home.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}

		if err != nil {
			return err //nolint:wrapcheck // the caller names the output
		}

		if !info.IsDir() {
			err = home.RemoveAll(dir)
			if err != nil {
				return err //nolint:wrapcheck // as above
			}

			continue
		}

		err = pruneDir(home, dir, listing[dir])
		if err != nil {
			return err
		}
	}

	return nil
}

func pruneDir(home *os.Root, dir string, keep map[string]bool) error {
	opened, err := home.Open(dir)
	if err != nil {
		return err //nolint:wrapcheck // the caller names the output
	}
	defer func() { _ = opened.Close() }()

	entries, err := opened.ReadDir(-1)
	if err != nil {
		return err //nolint:wrapcheck // as above
	}

	for _, entry := range entries {
		if keep[entry.Name()] {
			continue
		}

		err = home.RemoveAll(path.Join(dir, entry.Name()))
		if err != nil {
			return err //nolint:wrapcheck // as above
		}
	}

	return nil
}

// overlayOnto writes staging's entries into home: a directory is merged into whatever is there, anything else replaces what is there. Directory modes are set last, deepest first, so a read-only one does not refuse its own children.
func overlayOnto(stage, home *os.Root) error {
	var dirs []string

	err := fs.WalkDir(stage.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || rel == "." {
			return walkErr
		}

		if entry.IsDir() {
			dirs = append(dirs, rel)

			return mergeDir(home, rel)
		}

		return replaceEntry(stage, home, rel, entry)
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	for _, rel := range slices.Backward(dirs) {
		info, err := stage.Stat(rel)
		if err == nil {
			err = home.Chmod(rel, info.Mode().Perm())
		}

		if err != nil {
			return fmt.Errorf("%w", err)
		}
	}

	return nil
}

// mergeDir makes rel a directory at home, replacing whatever else stood there.
func mergeDir(home *os.Root, rel string) error {
	info, err := home.Lstat(rel)
	if err == nil && !info.IsDir() {
		err = home.Remove(rel)
	}

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err //nolint:wrapcheck // the caller names the output
	}

	return home.MkdirAll(rel, 0o700) //nolint:wrapcheck // as above
}

// replaceEntry puts staging's file or symlink at rel, in place of whatever stood there.
func replaceEntry(stage, home *os.Root, rel string, entry fs.DirEntry) error {
	err := home.RemoveAll(rel)
	if err != nil {
		return err //nolint:wrapcheck // the caller names the output
	}

	if entry.Type()&fs.ModeSymlink != 0 {
		target, err := stage.Readlink(rel)
		if err != nil {
			return err //nolint:wrapcheck // as above
		}

		return home.Symlink(target, rel) //nolint:wrapcheck // as above
	}

	info, err := entry.Info()
	if err != nil {
		return err //nolint:wrapcheck // as above
	}

	from, err := stage.Open(rel)
	if err != nil {
		return err //nolint:wrapcheck // as above
	}
	defer func() { _ = from.Close() }()

	to, err := home.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err //nolint:wrapcheck // as above
	}

	_, err = io.Copy(to, from)
	if err != nil {
		_ = to.Close()

		return err //nolint:wrapcheck // as above
	}

	return to.Close() //nolint:wrapcheck // as above
}
