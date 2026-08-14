package agent

// read_file: a whole file up to a cap, or a line range paged out of a big one.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jtarchie/steps/internal/shell"
)

// maxReadFileBytes caps how much read_file returns in a single call — both a
// plain read and a start_line/end_line slice. Unlike maxToolOutputBytes,
// read_file is never spilled to a NEW file (the file already exists on disk;
// spilling a read back out to another file would be a pointless loop), so
// this is a straight truncation budget with line ranges as the way to read
// further. It is intentionally much larger than maxToolOutputBytes so that a
// spilled tool output (always just over that, up to shell.SpillMaxBytes) can
// be read back whole in one call — reading a file is an explicit, intentional
// act by the model, unlike a command that floods output unbidden.
const maxReadFileBytes = 100_000

// maxReadFileScanBytes bounds the largest single line readFileRange will pull
// into memory before deciding what to keep. It's well above maxReadFileBytes
// (the return budget) so a spilled file's long line — base64/minified/`jq -c`
// output, which frequently has no newlines at all — is still readable
// (byte-truncated to the budget) rather than failing the scan with a cryptic
// "token too long"; a line even larger than this degrades to truncated=true,
// still not a hard error. Matches internal/shell's SpillMaxBytes.
const maxReadFileScanBytes = 10 << 20 // 10 MiB

// execReadFile resolves path and dispatches to readFileFull (read from the
// top, capped at maxReadFileBytes) or, when either start_line or end_line is
// supplied, readFileRange — a line-based slice so a large file (or a spilled
// tool output) can be paged through instead of only ever showing a prefix.
func execReadFile(_ context.Context, args map[string]any, env toolEnv) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		return map[string]any{"error": `read_file: missing required argument "path"`}
	}

	resolved, err := resolveAgentPath(env.dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	startLine, hasStart := intArg(args, "start_line")
	endLine, hasEnd := intArg(args, "end_line")

	if !hasStart && !hasEnd {
		return readFileFull(resolved)
	}

	if !hasStart {
		startLine = 1
	}

	if startLine < 1 {
		return map[string]any{"error": "read_file: start_line must be >= 1"}
	}

	if hasEnd && endLine < startLine {
		return map[string]any{"error": "read_file: end_line must be >= start_line"}
	}

	return readFileRange(resolved, startLine, endLine, hasEnd)
}

// readFileFull is read_file with no start_line/end_line. An over-cap file
// degrades to a plain truncation, restated in wording that matches
// shell.SpillPointerMessage's tag style (a leading block, not a trailing
// marker) so the two "output was cut" shapes read the same way, and pointing
// the model at start_line/end_line — the file's own paging mechanism.
func readFileFull(resolved string) map[string]any {
	f, err := os.Open(resolved) //nolint:gosec // resolveAgentPath rejects paths escaping dir, the step's own workspace
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	// Read at most maxReadFileBytes regardless of the file's real size, so a
	// huge file doesn't pay full allocation and I/O cost before the truncation
	// below would have discarded most of it anyway. stat.Size() (not the read
	// length) drives the message, so the reported byte count matches what it
	// would have said had it read the whole file.
	data, err := io.ReadAll(io.LimitReader(f, maxReadFileBytes))
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	content := string(data)
	if stat.Size() > maxReadFileBytes {
		content = fmt.Sprintf(
			"<file_truncated>\nThis file is %s, exceeding the %s inline read limit. Showing the first %s below. Use start_line/end_line to read further into the file.\n</file_truncated>\n\n%s",
			shell.FormatBytes(int(stat.Size())), shell.FormatBytes(maxReadFileBytes), shell.FormatBytes(len(data)), content,
		)
	}

	return map[string]any{"content": content}
}

// readFileRange reads resolved line by line, returning only lines
// [startLine, endLine] (1-indexed, inclusive; hasEnd false means "to EOF").
// Still capped at maxReadFileBytes — an unreasonably wide range on a huge
// file degrades to a truncated=true flag (content here is whole lines, not an
// arbitrary byte prefix) rather than buffering the whole slice unbounded.
func readFileRange(resolved string, startLine, endLine int, hasEnd bool) map[string]any {
	f, err := os.Open(resolved) //nolint:gosec // resolveAgentPath rejects paths escaping dir, the step's own workspace
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer func() { _ = f.Close() }()

	content, lastLine, truncated, err := scanLineRange(f, startLine, endLine, hasEnd)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	if lastLine == 0 {
		lastLine = startLine - 1
	}

	return map[string]any{
		"content":    content,
		"start_line": startLine,
		"end_line":   lastLine,
		"truncated":  truncated,
	}
}

// scanLineRange walks r line by line, accumulating lines [startLine, endLine]
// under the maxReadFileBytes budget. It returns the content, the last line
// actually included (0 if none), and whether anything was truncated. A line
// longer than the scan buffer degrades to truncated=true rather than a hard
// error — the spilled-long-line case this paging path exists to serve; only a
// genuine read error is returned.
func scanLineRange(r io.Reader, startLine, endLine int, hasEnd bool) (content string, lastLine int, truncated bool, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxReadFileScanBytes)

	content, lastLine, truncated = accumulateLineRange(scanner, startLine, endLine, hasEnd)

	scanErr := scanner.Err()
	if scanErr != nil && !errors.Is(scanErr, bufio.ErrTooLong) {
		return "", 0, false, fmt.Errorf("read_file: %w", scanErr)
	}

	if scanErr != nil {
		truncated = true // a line over the scan buffer bound: degrade, don't hard-error
	}

	return content, lastLine, truncated, nil
}

// accumulateLineRange drains scanner, collecting lines [startLine, endLine]
// under the maxReadFileBytes budget. The caller inspects scanner.Err()
// afterward — a too-long line is left for scanLineRange to classify.
func accumulateLineRange(scanner *bufio.Scanner, startLine, endLine int, hasEnd bool) (content string, lastLine int, truncated bool) {
	var (
		buf  strings.Builder
		line int
	)

	for scanner.Scan() {
		line++

		if line < startLine {
			continue
		}

		if hasEnd && line > endLine {
			break
		}

		included, stop, cut := appendRangeLine(&buf, scanner.Bytes())
		if cut {
			truncated = true
		}

		if included {
			lastLine = line
		}

		if stop {
			break
		}
	}

	return buf.String(), lastLine, truncated
}

// appendRangeLine adds one line's text to buf under the maxReadFileBytes
// return budget. It reports whether any of the line was included (so the
// caller can advance the last-line counter), whether to stop scanning, and
// whether anything was cut. An over-budget FIRST line keeps a byte-truncated
// prefix rather than nothing, so a single-long-line file (a common shape for
// spilled command output) is still partially readable; an over-budget later
// line is dropped whole, leaving the last full line as the paging cursor.
func appendRangeLine(buf *strings.Builder, text []byte) (included, stop, cut bool) {
	if buf.Len() == 0 {
		if len(text) > maxReadFileBytes {
			buf.Write(text[:maxReadFileBytes])

			return true, true, true
		}

		buf.Write(text)

		return true, false, false
	}

	if buf.Len()+len(text)+1 > maxReadFileBytes {
		return false, true, true
	}

	buf.WriteByte('\n')
	buf.Write(text)

	return true, false, false
}
