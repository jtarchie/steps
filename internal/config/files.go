package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Files is what a pipeline's includes (run_file:, system_file:,
// message_files:, file:) are read through. DirFS reads a directory on disk;
// Bundle reads a fixed, in-memory set — what a daemon resolves a `steps
// pipeline set` upload against, since it has no sibling filesystem of its
// own to read the sender's includes from.
type Files interface {
	ReadFile(name string) ([]byte, error)
}

// DirFS reads files relative to a directory on disk. ".." is allowed
// deliberately: the pipeline file is trusted input, and a file placed beside
// it by the same author is at the same trust level — a shared ../tasks/
// directory next to a pipelines/ directory is a legitimate layout, not a hole
// to close. See includeResolver.readFile. This is why DirFS is a thin
// os.ReadFile(filepath.Join(...)) rather than an fs.FS: fs.FS's ValidPath
// refuses any name containing "..".
type DirFS string

// ReadFile reads name relative to the directory d wraps.
func (d DirFS) ReadFile(name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(string(d), name)) //nolint:gosec // trusted input, see DirFS doc comment
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return data, nil
}

// Bundle is a CLOSED, in-memory set of include contents, keyed by the
// pipeline-relative path the revision hashes — that is, cleaned by
// filepath.Clean, which is the form Revision.Includes carries and therefore
// the form a sender builds its keys from.
//
// Closed deliberately: a daemon resolving an include it was not sent must
// fail, never fall back to reading its own filesystem — otherwise a crafted
// run_file: could read daemon-local files the sender never uploaded. There is
// no fallback branch here to remove; that is the point.
type Bundle map[string]string

// ReadFile returns the bundled content for name, or a not-exist error the
// same callers already handle for a missing file on disk.
//
// name is cleaned before the lookup because a map is an EXACT match where
// DirFS is not: filepath.Join silently cleans, so `run_file: ./ci/unit.sh`
// reads off disk and is recorded in the revision as ci/unit.sh. A sender
// keying its bundle off Revision.Includes — the only list it has — would then
// hand the daemon ci/unit.sh for an include the resolver asks for as
// ./ci/unit.sh, and every such `set` would be refused for a file it did
// upload.
func (b Bundle) ReadFile(name string) ([]byte, error) {
	content, ok := b[filepath.Clean(name)]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, os.ErrNotExist)
	}

	return []byte(content), nil
}

// describe names file as the person reading an error would recognise it:
// joined onto the directory a DirFS wraps, so a local load still cites the
// path that was typed, and bare otherwise — a daemon resolving a Bundle has
// no directory of its own to cite, and citing one would be a lie about where
// the bytes came from.
func describe(fsys Files, file string) string {
	if dir, ok := fsys.(DirFS); ok {
		return filepath.Join(string(dir), file)
	}

	return file
}

// origin names WHERE fsys reads from, for the "no such file" hint an author
// has to act on.
func origin(fsys Files) string {
	if dir, ok := fsys.(DirFS); ok {
		return fmt.Sprintf("the pipeline directory %q", string(dir))
	}

	return "the uploaded pipeline configuration"
}

// maxPipelineNameLength bounds a pipeline name — it becomes a URL path
// segment and a database identity, not free text.
const maxPipelineNameLength = 200

// ValidPipelineName reports whether name is safe to use as a URL path
// segment and a stored identity: non-empty, bounded, and drawn from
// [A-Za-z0-9._-] with "." and ".." refused outright.
//
// An allow-list rather than a deny-list, because the name is CONCATENATED
// into URLs rather than escaped into them ("/p/" + slug, in internal/web),
// and the characters that break that are not the obvious ones: "?" and "#"
// truncate a link into a query or a fragment, "%" makes the router decode a
// segment the name never contained, and ".." is a path traversal in a route
// that takes the name from a request. Rejecting the whole class costs an
// operator nothing — this is the same set Concourse validates a pipeline
// identifier against.
//
// Unlike a filename-derived identity, a set-supplied name was never
// validated before — see Slugify, which only ever produced one.
func ValidPipelineName(name string) error {
	if name == "" {
		return errors.New("pipeline name must not be empty")
	}

	if len(name) > maxPipelineNameLength {
		return fmt.Errorf("pipeline name must be %d characters or fewer", maxPipelineNameLength)
	}

	if name == "." || name == ".." {
		return fmt.Errorf("pipeline name %q is not valid", name)
	}

	for _, r := range name {
		if !validNameRune(r) {
			return fmt.Errorf("pipeline name %q must contain only letters, digits, %q, %q or %q", name, ".", "_", "-")
		}
	}

	return nil
}

func validNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-':
		return true
	default:
		return false
	}
}
