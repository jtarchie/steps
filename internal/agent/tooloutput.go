package agent

// What a tool is allowed to hand back inline, and where the rest goes.

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jtarchie/steps/internal/shell"
)

// maxToolOutputBytes caps how much of a tool's textual output (command
// stdout/stderr, an MCP tool's response, a sub-agent's final answer, a fix
// loop's failure output) is returned to the model inline. A runaway command
// or a chatty MCP server would otherwise flood the model's context window.
// Anything beyond this is spilled to a file under the step's spill directory
// instead of being dropped — see spillOrTruncate.
//
// Deliberately smaller than maxReadFileBytes: a spilled file exists precisely
// so the model can pull it back with read_file, so the read-back budget must
// be larger than the spill threshold, otherwise a file just over this size
// could never be read back in one call and the model loops re-reading a
// truncated prefix (the exact regression that motivated splitting the two
// constants apart).
const maxToolOutputBytes = 32_000

// outputLimit resolves a grant's max_output_bytes: — unset takes the global
// default, any explicit value wins in either direction, bounded above by the
// spill ceiling. Raising it is a declared trade: the pipeline author chose a
// bigger inline result over a spill pointer for a tool whose output the model
// genuinely needs whole. Lowering it loses no data — overflow still spills to
// a file the model can read back — it only shrinks what lands inline.
func outputLimit(specified int) int {
	if specified <= 0 {
		return maxToolOutputBytes
	}

	return min(specified, shell.SpillMaxBytes)
}

// truncateToolOutput caps s at maxToolOutputBytes, appending a marker when it
// cuts. Used directly only by read_file (spilling a file read back out to
// another file would be a pointless loop); every other oversized-output site
// goes through spillOrTruncate, which falls back to this only when spilling
// itself isn't possible.
func truncateToolOutput(s string) string {
	return truncateToolOutputLimit(s, maxToolOutputBytes)
}

// truncateToolOutputLimit is truncateToolOutput with an explicit budget, for
// a grant that tuned its own via max_output_bytes:.
func truncateToolOutputLimit(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	return s[:limit] + fmt.Sprintf("\n... [truncated %d bytes]", len(s)-limit)
}

// spillOrTruncate is the one-shot counterpart to shellToolResult's streaming
// spill: a caller that already holds its full result as a string — an MCP
// tool's content, a sub-agent's final answer, a fix loop's failure output —
// uses this so oversized output is saved to a file the model can read back,
// not dropped.
func spillOrTruncate(content string, spillDir string) string {
	return spillOrTruncateLimit(content, maxToolOutputBytes, spillDir)
}

// spillOrTruncateLimit is spillOrTruncate with an explicit inline budget, so
// a tool grant that narrowed its own via max_output_bytes: spills sooner.
// Nothing is lost by a lower limit — the full content still reaches the spill
// file — only the inline share shrinks.
func spillOrTruncateLimit(content string, limit int, spillDir string) string {
	if len(content) <= limit {
		return content
	}

	path, ok := spillToFile(content, spillDir)
	if !ok {
		return truncateToolOutputLimit(content, limit)
	}

	return shell.SpillPointerMessage(len(content), path, spillPreview(content))
}

// spillToFile writes content to a new "output-*.txt" file under spillDir
// (the same naming spillWriter uses, so spilled files look uniform however
// they were produced), returning the file's path. ok is false when spillDir
// is unset or any create/write/close step failed — spilling is a usability
// improvement, never something a tool call should fail over, so every caller
// degrades rather than erroring. Split out so a caller that must NOT fall
// back to a byte prefix — boundedStructuredContent, whose content is
// serialized JSON — can branch on ok explicitly.
func spillToFile(content string, spillDir string) (string, bool) {
	if spillDir == "" {
		return "", false
	}

	f, err := os.CreateTemp(spillDir, "output-*.txt")
	if err != nil {
		slog.Warn("agent.spill_output", "error", err)

		return "", false
	}

	_, writeErr := f.WriteString(content)
	closeErr := f.Close()

	if writeErr != nil || closeErr != nil {
		_ = os.Remove(f.Name())
		slog.Warn("agent.spill_output", "write_error", writeErr, "close_error", closeErr)

		return "", false
	}

	return f.Name(), true
}

// spillPreview returns the head of content that accompanies a spill pointer
// message, bounded by shell.SpillPreviewBytes.
func spillPreview(content string) []byte {
	preview := []byte(content)
	if len(preview) > shell.SpillPreviewBytes {
		preview = preview[:shell.SpillPreviewBytes]
	}

	return preview
}

// shellToolResult builds the FunctionResponse map for a shell-backed tool
// (run_shell and every custom tool). It executes command through env.runner —
// the host or, when the step's image: is set, the step's container — with
// env.dir as cwd, via RunCaptureFullLimitedStreamed so a runaway command's
// output is capped as it's captured rather than fully buffered, AND streamed
// live (prefixed with the agent's name). A model-directed shell command was
// previously invisible until the agent's final text response; this makes it
// watchable as it runs, the same way a task's run: step already is. When
// env.spillDir is set, output beyond the cap is streamed to a file under it
// and the model gets a pointer message instead of losing the overflow.
func shellToolResult(ctx context.Context, command string, env toolEnv, limit int) map[string]any {
	stdout, stderr, exitCode, err := env.runner.RunCaptureFullLimitedStreamed(ctx, command, limit, env.spillDir)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	return map[string]any{
		"stdout":    stdout,
		"stderr":    stderr,
		"exit_code": exitCode,
	}
}
