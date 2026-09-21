package agent

// The file tools reaching the step's tree through the image's own userland, which is what makes a placed agent whole: its run_shell already execs into one container on the worker, and routing the file tools into that same container is what puts both on one copy of the tree.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/shell"
)

// containerTree runs each file operation as a POSIX sh script in the step's container, through the same shell.Runner its run_shell uses. Nothing is mounted and nothing is pushed: the scripts use only the commands treeCommands names — which probeUserland checks for before a token is spent — plus printf, a shell builtin everywhere. That is why they work on a foreign machine's image without steps having to put anything there.
type containerTree struct {
	runner shell.Runner
	dir    string
	lost   *lostTree
}

func (c containerTree) root() string { return c.dir }

// maxScriptOutputBytes bounds what one script may hand back. Generous, because a read_file of a spilled output is legitimately large, and enforced in the container by head -c so an overrun is discarded there rather than paid for across a worker's tunnel.
const maxScriptOutputBytes = 12 << 20

// read executes a script that cannot change the tree, which is what lets a placed agent's dozens of reads avoid fetching the step's outputs once each — see shell.ReadOnly.
func (c containerTree) read(ctx context.Context, script string) (stdout string, code int, err error) {
	stdout, _, code, err = c.readDiag(ctx, script)

	return stdout, code, err
}

// readDiag is read, keeping the script's stderr as well. Only grep needs it, and it needs it because its output goes through `head`: the script's exit status is then head's, so a grep that REFUSED the pattern is otherwise indistinguishable from a grep that matched nothing.
func (c containerTree) readDiag(ctx context.Context, script string) (stdout, stderr string, code int, err error) {
	return c.execute(shell.ReadOnly(ctx), script)
}

// run executes one script and returns its stdout. A nonzero exit is the script's own answer and comes back as data in the second return; only a failure to run at all is an error, which is the same line shell.Runner draws and the one the conversation cannot recover from.
func (c containerTree) run(ctx context.Context, script string) (stdout string, code int, err error) {
	stdout, _, code, err = c.execute(ctx, script)

	return stdout, code, err
}

func (c containerTree) execute(ctx context.Context, script string) (stdout, stderr string, code int, err error) {
	out, errOut, code, err := c.runner.RunCaptureFullLimited(ctx, script, maxScriptOutputBytes, "")
	if err != nil {
		lost := fmt.Errorf("%w: %w", errNoTreeAccess, err)
		c.lost.note(lost)

		return "", "", 0, lost
	}

	return out, errOut, code, nil
}

// statScript prints a kind letter and a byte size, or exits 1 for a path that is not there. Size comes from wc -c rather than from ls, whose long-format columns differ between busybox and GNU in exactly the fields a parser would need.
func (c containerTree) stat(ctx context.Context, p string) (treeStat, error) {
	script := fmt.Sprintf(`p=%s; if [ -d "$p" ]; then printf 'd 0\n'; elif [ -e "$p" ]; then printf 'f %%s\n' "$(wc -c < "$p")"; else exit 1; fi`, shQuote(p))

	out, code, err := c.read(ctx, script)
	if err != nil {
		return treeStat{}, err
	}

	if code != 0 {
		return treeStat{}, fmt.Errorf("stat %s: %w", p, os.ErrNotExist)
	}

	fields := strings.Fields(out)
	if len(fields) != 2 {
		return treeStat{}, fmt.Errorf("%w: stat of %q answered %q", errNoTreeAccess, p, out)
	}

	size, _ := strconv.ParseInt(fields[1], 10, 64)

	return treeStat{size: size, isDir: fields[0] == "d"}, nil
}

func (c containerTree) readBytes(ctx context.Context, p string, limit int64) ([]byte, error) {
	script := fmt.Sprintf(`p=%s; [ -f "$p" ] || exit 1; head -c %d -- "$p"`, shQuote(p), limit)

	out, code, err := c.read(ctx, script)
	if err != nil {
		return nil, err
	}

	if code != 0 {
		return nil, fmt.Errorf("open %s: %w", p, os.ErrNotExist)
	}

	return []byte(out), nil
}

// openRange asks sed for the lines the caller will actually keep, which is the one place the container can do less work than the host rather than more: a range deep in a large file crosses the wire as the range, not as the file.
func (c containerTree) openRange(ctx context.Context, p string, through int) (io.ReadCloser, error) {
	body := fmt.Sprintf(`head -c %d -- "$p"`, int64(maxReadFileScanBytes))
	if through > 0 {
		body = fmt.Sprintf(`sed -n '1,%dp' -- "$p" | head -c %d`, through, int64(maxReadFileScanBytes))
	}

	script := fmt.Sprintf(`p=%s; [ -f "$p" ] || exit 1; %s`, shQuote(p), body)

	out, code, err := c.read(ctx, script)
	if err != nil {
		return nil, err
	}

	if code != 0 {
		return nil, fmt.Errorf("open %s: %w", p, os.ErrNotExist)
	}

	return io.NopCloser(strings.NewReader(out)), nil
}

// maxWriteChunkBytes bounds one script's share of a write once quoted, keeping the command well inside a kernel's argument limit. A write larger than this becomes several appends into a staging file, so the size a model may write is not capped by what one exec can carry.
const maxWriteChunkBytes = 24 << 10

// writeFile stages the content and finishes with `cat staged > target`, never a move. The redirection writes THROUGH the existing inode, so the file keeps its mode; a rename would replace it and quietly strip the executable bit off a checked-in script.
func (c containerTree) writeFile(ctx context.Context, p string, data []byte, appendTo bool) error {
	if bytes.IndexByte(data, 0) >= 0 {
		return errNULInContent
	}

	staged := path.Join(path.Dir(p), ".steps-write-"+strconv.Itoa(os.Getpid()))

	scripts := writeScripts(p, staged, string(data), appendTo)
	for i, script := range scripts {
		// Only the LAST script lands anything the step keeps; the ones before it fill a staging file the last one removes. Running them as reads is what stops a large write from paying a full transfer of the step's outputs once per 24KiB chunk over a worker's tunnel.
		exec := c.read
		if i == len(scripts)-1 {
			exec = c.run
		}

		out, code, err := exec(ctx, script)
		if err != nil {
			return err
		}

		if code != 0 {
			return fmt.Errorf("%w: writing %s: %s", errWriteFailed, p, strings.TrimSpace(out))
		}
	}

	return nil
}

var (
	errNULInContent = errors.New("content contains a NUL byte, which cannot cross into a container as a shell argument")
	errWriteFailed  = errors.New("write")
)

// writeScripts is the staging write as a series of commands, each bounded by what one argument list can hold.
func writeScripts(target, staged, content string, appendTo bool) []string {
	quoted := chunkQuoted(content)

	scripts := make([]string, 0, len(quoted)+1)
	scripts = append(scripts, fmt.Sprintf(`mkdir -p -- %s && : > %s`, shQuote(path.Dir(target)), shQuote(staged)))

	for _, chunk := range quoted {
		scripts = append(scripts, fmt.Sprintf(`printf '%%s' %s >> %s`, chunk, shQuote(staged)))
	}

	redirect := ">"
	if appendTo {
		redirect = ">>"
	}

	// The staging file is removed whatever the write did. Chained behind `&&` it survived every failure, inside the step's own tree, where a declared output would have carried it home.
	scripts = append(scripts, fmt.Sprintf(`{ [ -e %[1]s ] || { : > %[1]s && chmod %[3]o %[1]s; }; } && cat %[2]s %[4]s %[1]s; s=$?; rm -f %[2]s; exit $s`,
		shQuote(target), shQuote(staged), defaultToolFileMode, redirect))

	return scripts
}

// chunkQuoted splits content into already-quoted pieces, measuring the QUOTED length: a chunk of quote characters quadruples on the way in, so splitting the raw bytes would still hand the kernel an argument list several times the size the split was chosen for.
func chunkQuoted(content string) []string {
	if content == "" {
		return []string{shQuote("")}
	}

	var (
		out   []string
		start int
		size  int
	)

	for i, r := range content {
		cost := 1
		if r == '\'' {
			cost = 4
		}

		if size+cost > maxWriteChunkBytes && i > start {
			out = append(out, shQuote(content[start:i]))
			start, size = i, 0
		}

		size += cost
	}

	return append(out, shQuote(content[start:]))
}

// listDir emits one tab-separated row per entry. find does the walking so a name with spaces survives, and the type comes from a test rather than from parsing ls, whose long format is not the same program to program.
func (c containerTree) listDir(ctx context.Context, p string) ([]treeEntry, error) {
	script := fmt.Sprintf(`p=%s; [ -d "$p" ] || exit 1; find "$p" -maxdepth 1 -mindepth 1 -exec sh -c 'for e do if [ -d "$e" ]; then printf "d\t0\t%%s\n" "$e"; else printf "f\t%%s\t%%s\n" "$(wc -c < "$e" 2>/dev/null || echo 0)" "$e"; fi; done' sh {} +`, shQuote(p))

	out, code, err := c.read(ctx, script)
	if err != nil {
		return nil, err
	}

	if code != 0 {
		return nil, fmt.Errorf("open %s: %w", p, os.ErrNotExist)
	}

	entries := parseListRows(out, p)

	// os.ReadDir sorts, and list_dir caps what it returns at maxListDirEntries — so an unordered find would show the model a different slice of the same directory depending on where its agent ran.
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	return entries, nil
}

// parseListRows turns the script's rows into entries, dropping anything malformed rather than failing the call: a single unreadable name is not a reason the model cannot see the rest of the directory.
func parseListRows(out, base string) []treeEntry {
	entries := []treeEntry{}

	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}

		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}

		size, _ := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)

		name := strings.TrimPrefix(fields[2], base+"/")
		if name == fields[2] {
			name = path.Base(fields[2])
		}

		entries = append(entries, treeEntry{name: name, isDir: fields[0] == "d", size: size})
	}

	return entries
}
