package agent

// How a file tool reaches the step's directory: directly, or through the image's own userland in the step's container.

import (
	"context"
	"errors"
	"io"
	"strings"
)

// tree is the IO half of the file tools. Every cap, glob, prune and result shape stays in the callers, so the two implementations differ only in how bytes are fetched and never in what the model is told — which is the whole promise, since an agent that moves to a worker must not start answering differently.
type tree interface {
	// root is the directory paths resolve against, in this tree's own spelling. For a container on a worker that is the worker's path, which is also what the model is told its working directory is.
	root() string
	// resolve confines rel to root and answers the absolute path in this tree's spelling.
	resolve(ctx context.Context, rel string) (string, error)
	// resolveWrite is resolve for a path that need not exist yet: the closest existing ancestor is what gets checked against root, since a missing leaf has no link to follow. Missing parents are created by whichever caller goes on to write.
	resolveWrite(ctx context.Context, rel string) (string, error)
	// stat answers os.ErrNotExist for a missing path, like os.Stat.
	stat(ctx context.Context, path string) (treeStat, error)
	// readBytes is the first limit bytes of a file.
	readBytes(ctx context.Context, path string, limit int64) ([]byte, error)
	// openRange is a reader over the file's first through lines, or over the whole file when through is zero. Both implementations start at line one, so the caller's scanner counts the same lines either way.
	openRange(ctx context.Context, path string, through int) (io.ReadCloser, error)
	// writeFile replaces path's contents, keeping the file's mode when it already exists.
	writeFile(ctx context.Context, path string, data []byte, appendTo bool) error
	// listDir is one directory's entries, unsorted, as the tool reports them.
	listDir(ctx context.Context, path string) ([]treeEntry, error)
	// search walks base under opts and returns what the model is shown.
	search(ctx context.Context, base string, opts searchOpts) (searchResult, error)
}

// treeStat is the part of os.FileInfo the file tools actually ask for. No mode: an edit keeps a file's own by writing THROUGH it (see hostTree.writeFile and containerTree.writeFile), so nothing has to carry one across.
type treeStat struct {
	size  int64
	isDir bool
}

// treeEntry is one list_dir row.
type treeEntry struct {
	name  string
	isDir bool
	size  int64
}

// errNoTreeAccess is a tree operation that could not run at all — a container that will not start, a worker that went away. Distinct from a tool's own failure, since the conversation cannot recover from it by trying a different path.
var errNoTreeAccess = errors.New("the step's container could not be reached")

// shQuote renders s as one POSIX sh word. Single quotes take everything literally except a single quote itself, which is closed, escaped and reopened — so a model-authored path or pattern crosses into the container as data and can never be read as syntax. This is what lets the file tools reach a container over the existing shell.Runner, with no argv exec and no new frame on the wire.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
