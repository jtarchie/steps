package tree

// The file tools reaching this process's own filesystem — what every agent without image: does, unchanged.

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Host reads and writes the step directory directly. It is the behaviour these tools had before a container could hold them, kept whole so an agent with no image: is byte-for-byte unaffected.
type Host struct{ Dir string }

// Root satisfies Tree.
func (h Host) Root() string { return h.Dir }

// Resolve satisfies Tree.
func (h Host) Resolve(_ context.Context, rel string) (string, error) {
	return ResolvePath(h.Dir, rel)
}

// ResolveWrite satisfies Tree.
func (h Host) ResolveWrite(_ context.Context, rel string) (string, error) {
	return ResolveWritePath(h.Dir, rel)
}

// Stat satisfies Tree.
func (h Host) Stat(_ context.Context, path string) (Stat, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Stat{}, fmt.Errorf("%w", err)
	}

	return Stat{Size: info.Size(), IsDir: info.IsDir()}, nil
}

// ReadBytes satisfies Tree.
func (h Host) ReadBytes(_ context.Context, path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // resolve confines path to the step's own workspace
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return data, nil
}

// OpenRange hands back the file itself and ignores through: the scanner the caller runs is what finds the lines, and on this side there is nothing cheaper to do than let it pass over them. The container implementation has a reason to honour it, and the caller cannot tell the difference because both start at line one.
func (h Host) OpenRange(_ context.Context, path string, _ int) (io.ReadCloser, error) {
	f, err := os.Open(path) //nolint:gosec // resolve confines path to the step's own workspace
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return f, nil
}

// WriteFile passes defaultToolFileMode for a file that does not exist yet; an existing one keeps the mode it has, since the OS ignores the permission argument once the inode is there. That is what stops an edit to a checked-in script from stripping its executable bit.
func (h Host) WriteFile(_ context.Context, path string, data []byte, appendTo bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if appendTo {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}

	f, err := os.OpenFile(path, flags, defaultToolFileMode) //nolint:gosec // resolveWrite rejects paths escaping dir
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = f.Close() }()

	_, err = f.Write(data)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// ListDir satisfies Tree.
func (h Host) ListDir(_ context.Context, path string) ([]Entry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	out := make([]Entry, 0, len(entries))

	for _, e := range entries {
		size := int64(0)

		info, infoErr := e.Info()
		if infoErr == nil {
			size = info.Size()
		}

		out = append(out, Entry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}

	return out, nil
}

// Search satisfies Tree.
func (h Host) Search(_ context.Context, base string, opts SearchOpts) (SearchResult, error) {
	return searchWalk(base, opts)
}

// defaultToolFileMode is what a file the model creates is born with. Stated rather than left to a umask so a placed agent and a local one produce the same thing.
const defaultToolFileMode = 0o644
