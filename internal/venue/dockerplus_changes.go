package venue

// Bringing home only what a step changed in an input it also declared as an output (steps#246 S6).

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// opaqueScript prints, NUL-separated, each directory of the upper layer overlay marks opaque: one removed and made again, which hides what the lower held under it without a whiteout per name. getfattr prints names unescaped, so a batch's output is only a filter, and each directory of a batch that might hold one is asked alone, where the exit status says whether the marker is there at all.
const opaqueScript = `cd /u && find . -type d -exec sh -c 'getfattr -n trusted.overlay.opaque "$@" 2>/dev/null | grep -q "=\"y\"\$" || exit 0; for d; do v=$(getfattr -n trusted.overlay.opaque "$d" 2>/dev/null) && case $v in *"=\"y\"") printf "%s\0" "$d";; esac; done; exit 0' sh {} +`

// fetchChanges brings an input-as-output home as its overlay's upper layer: the files the step wrote, and the deletions the overlay recorded, instead of the whole merged tree.
func (s *plusSession) fetchChanges(ctx context.Context, name, upper string) error {
	opaque, err := s.opaqueDirs(ctx, upper)
	if err != nil {
		return fmt.Errorf("fetching %s's changes: %w", name, err)
	}

	staging, err := os.MkdirTemp(s.spec.Cwd, ".steps-fetch-")
	if err != nil {
		return fmt.Errorf("fetching %s's changes: %w", name, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	deleted, err := s.fetchUpper(ctx, upper, staging)
	if err != nil {
		return fmt.Errorf("fetching %s's changes: %w", name, err)
	}

	// Everything has arrived before anything at home is touched, so a failed transfer leaves the last fetch's tree.
	err = applyChanges(staging, filepath.Join(s.spec.Cwd, name), slices.Concat(deleted, opaque))
	if err != nil {
		return fmt.Errorf("fetching %s's changes: %w", name, err)
	}

	return nil
}

// applyChanges removes each gone path at home, then writes staging over it, both confined by os.Root. An opaque directory counts as gone, since staging holds all of it that survived.
func applyChanges(staging, dst string, gone []string) error {
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

	for _, rel := range gone {
		err = home.RemoveAll(rel)
		if err != nil && !errors.Is(err, syscall.ENOTDIR) {
			return fmt.Errorf("%w", err)
		}
	}

	err = overlayOnto(stage, home)
	if err != nil {
		return err
	}

	return checkHomeLinks(home)
}

// checkHomeLinks holds every link in home, not only staged ones, to the fetch's landing rule: staging lacks the unchanged tree a link can chain through (a/up -> ..), and a changed link can make an untouched one climb out. An escaping link is removed, since home already holds it and a hook may still read the output.
func checkHomeLinks(home *os.Root) error {
	var escaped error

	err := fs.WalkDir(home.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.Type()&fs.ModeSymlink == 0 {
			return walkErr
		}

		landing := wire.CheckLanding(home, rel)
		if landing == nil {
			return nil
		}

		escaped = errors.Join(escaped, landing)

		return home.Remove(rel)
	})

	return errors.Join(escaped, err)
}

// opaqueDirs runs opaqueScript over the upper layer, read-only. Without CAP_SYS_ADMIN every trusted.* xattr reads as absent, and a recreated directory would keep at home what the step removed from it.
func (s *plusSession) opaqueDirs(ctx context.Context, upper string) ([]string, error) {
	code, stdout, stderr, err := s.docker.RunOnce(ctx, dockerapi.ContainerSpec{
		Image:   treedigest.Image,
		Cmd:     []string{"sh", "-c", opaqueScript},
		Labels:  shell.OwnershipLabels(),
		Mounts:  []dockerapi.Bind{volumeMount(upper, "/u", true)},
		Network: "none",
		CapAdd:  []string{"SYS_ADMIN"},
		// Each name removes a directory at home, so a cut listing fails rather than parses.
		MaxOutput: deletionsMaxBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	if code != 0 {
		return nil, fmt.Errorf("%w: exit %d: %s", errListingFailed, code, stderr)
	}

	s.received.Add(int64(len(stdout)))

	return parseOpaque(stdout)
}

// deletionsMaxBytes bounds the deleted names held in memory, whiteouts and opaque directories each: about two million names at a hundred bytes each.
const deletionsMaxBytes = 256 << 20

// errListingFailed is the listing container saying no.
var errListingFailed = errors.New("listing the recreated directories failed")

// errBadDeletion is a deletion naming a path outside the tree.
var errBadDeletion = errors.New("a deletion names a path outside the tree")

// errTooManyDeletions is a step whose whiteouts outgrew deletionsMaxBytes: removing only some of them would leave home neither tree.
var errTooManyDeletions = errors.New("too many deletions to hold")

func parseOpaque(out string) ([]string, error) {
	var dirs []string

	for record := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		if record == "" {
			continue
		}

		dir, err := deletedPath(record)
		if err != nil {
			return nil, err
		}

		dirs = append(dirs, dir)
	}

	return dirs, nil
}

// deletedPath holds a name the worker sent to staying inside the tree, since it decides what is removed at home.
func deletedPath(name string) (string, error) {
	rel := path.Clean(name)
	if !filepath.IsLocal(rel) || rel == "." {
		return "", fmt.Errorf("%w: %q", errBadDeletion, name)
	}

	return rel, nil
}

// fetchUpper copies the upper layer into staging, minus overlay's whiteouts, and returns the paths those whiteouts name.
func (s *plusSession) fetchUpper(ctx context.Context, upper, staging string) ([]string, error) {
	holder, err := s.docker.CreateHolder(ctx, "steps-upper-read-"+randomSuffix(), treedigest.Image, shell.OwnershipLabels(), []dockerapi.Bind{volumeMount(upper, holderMount, true)})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		_ = s.docker.RemoveContainer(removeCtx, holder)
	}()

	content, err := s.docker.GetArchive(ctx, holder, holderMount+"/.")
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { _ = content.Close() }()

	reader, writer := io.Pipe()

	var deleted []string

	done := make(chan struct{})

	go func() {
		defer close(done)

		var err error

		// A refusal reaches the unpack through the pipe, so it fails the fetch before anything at home is touched.
		deleted, err = dropWhiteouts(&byteCounter{r: content, n: &s.received}, writer, deletionsMaxBytes)
		writer.CloseWithError(err)
	}()

	err = wire.UnpackFetchedTree(reader, staging, fetchLimits())
	_ = reader.CloseWithError(err)

	<-done

	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return deleted, nil
}

// dropWhiteouts copies a tar stream, leaving out character devices (no step output is ever one), and returns the paths its 0:0 ones, overlay's whiteouts, say were deleted. Since Linux 5.8 overlayfs makes every whiteout after the first a hard link to one shared inode, so the archive carries the first deletion as a device and each later one as a hard link to it; those are deletions too, or a step that deleted two files failed to come home.
func dropWhiteouts(from io.Reader, to io.Writer, maxBytes int) ([]string, error) {
	in, out := tar.NewReader(from), tar.NewWriter(to)
	set := whiteoutSet{seen: map[string]bool{}, maxBytes: maxBytes}

	for {
		header, err := in.Next()
		if errors.Is(err, io.EOF) {
			return set.deleted, out.Close() //nolint:wrapcheck // the unpack names the tree
		}

		if err != nil {
			return nil, err //nolint:wrapcheck // as above
		}

		dropped, err := set.add(header)
		if err != nil {
			return nil, err
		}

		if dropped {
			continue
		}

		err = out.WriteHeader(header)
		if err != nil {
			return nil, err //nolint:wrapcheck // as above
		}

		_, err = io.Copy(out, in) //nolint:gosec // bounded by the unpack's own limits
		if err != nil {
			return nil, err //nolint:wrapcheck // as above
		}
	}
}

type whiteoutSet struct {
	seen     map[string]bool
	deleted  []string
	held     int
	maxBytes int
}

func (w *whiteoutSet) add(header *tar.Header) (bool, error) {
	whiteout := header.Typeflag == tar.TypeChar && header.Devmajor == 0 && header.Devminor == 0
	if !whiteout && (header.Typeflag != tar.TypeLink || !w.seen[path.Clean(header.Linkname)]) {
		return header.Typeflag == tar.TypeChar, nil
	}

	rel, err := deletedPath(header.Name)
	if err != nil {
		return false, err
	}

	w.held += len(rel)
	if w.held > w.maxBytes {
		return false, errTooManyDeletions
	}

	w.seen[rel] = true
	w.deleted = append(w.deleted, rel)

	return true, nil
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
