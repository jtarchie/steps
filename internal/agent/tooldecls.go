package agent

// The built-in tool catalogue: what each one is called, what it claims to do,
// and which impl runs it.

import (
	"fmt"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

type builtinTool struct {
	decl *genai.FunctionDeclaration
	impl toolImpl
}

// toolDescription returns the description text for a built-in tool, loading
// from the embedded library when available and falling back to a Go constant.
// The embedded files are the canonical source; this fallback ensures old
// binaries without matching embedded files still work.
func toolDescription(name, fallback string) string {
	desc, err := config.ReadBuiltinToolDescription(name)
	if err == nil && desc != "" {
		return desc
	}

	return fallback
}

// runShellDescription is run_shell's base description, extended when the
// step is containerized (image != "") to say where the command runs.
//
// It used to carry a warning that each call was a fresh container and that
// nothing persisted between them. That warning is gone because the fact is:
// a step's calls now share one container (see internal/shell's
// dockerSession), so state behaves the way it does on the host. What is left
// is the one thing a model still benefits from knowing — that it is inside an
// image, which explains why the toolchain it expects may simply not be there.
func runShellDescription(image string) string {
	desc := "Run a shell command via `sh -c`, with cwd set to the step's working directory. Returns stdout, stderr, and exit_code." +
		" If a stream's output is too large to return inline, it's instead saved to a file under the working directory and a" +
		" pointer message (an absolute path, size, and a preview) is returned in its place — read that file back with" +
		" run_shell (e.g. grep/sed on the absolute path), or with read_file, which accepts the absolute path from the" +
		" pointer message directly, using start_line/end_line to page through it."
	if image != "" {
		desc += fmt.Sprintf(" Runs inside the %s container image, not on the host — only what that image provides is available."+
			" Every call in this step shares one container, so a package you install, a variable you export, or a directory you cd into stays in effect for later calls.", image)
	}

	return desc
}

// listDirDescription documents list_dir's entry cap, built with fmt.Sprintf
// (rather than a hardcoded number in a string literal) so the description can
// never silently drift from maxListDirEntries itself.
//
//nolint:gochecknoglobals // computed once from a const; not a mutable global
var listDirDescription = fmt.Sprintf(
	`List entries (name, is_dir, size) in a directory, given a path relative to the working directory. Defaults to "." if omitted. Capped at the first %d entries for a very large directory — the result's "total"/"truncated" fields say whether more exist.`,
	maxListDirEntries,
)

// readFileDescription documents read_file's two modes: a plain call (no
// start_line/end_line) reads from the top of the file, capped at
// maxReadFileBytes; passing either turns it into a line-based slice instead.
const readFileDescription = "Read a UTF-8 text file's contents, given a path relative to the step's working directory." +
	" Optionally pass start_line and/or end_line (1-indexed, inclusive) to read only a slice of a large file instead of" +
	" its capped prefix — useful both for a file too big to read in one call and for output any tool spilled to a file" +
	" when it exceeded the inline size limit (run_shell, an MCP tool, a sub-agent's answer, ...) — see" +
	" that tool's own result for the exact path."

// writeFileDescription documents write_file's contract.
const writeFileDescription = "Write text content to a file, given a path relative to the step's working directory." +
	" Creates the file if it doesn't exist, overwriting any existing content unless append is true." +
	" Missing parent directories are created automatically."

// editFileDescription documents edit_file's contract. Every failure mode it
// names is recoverable on the model's next turn — "read the file again and
// copy the text exactly", "include more surrounding lines" — matching
// execCustomTool's missing-argument posture, since a local model recovering
// from a bad edit in-conversation is far cheaper than a burned attempt.
//
// The pairing with read_file is load-bearing and deliberate: read_file
// returns RAW bytes, so text a model copies out of one is a byte-exact
// old_string here. Adding line numbers to read_file would break that.
// search_files' content mode is where line numbers come from instead.
//
// The forgiving chain (editfile.go) is mentioned, but as a safety net, not
// an invitation to paraphrase: a verbatim copy still lands as "exact",
// which is the only match_mode the model should aim for.
const editFileDescription = "Replace an exact string in a text file — the way to change part of a file without" +
	" re-emitting the whole thing. path is relative to the step's working directory (or an absolute path inside it)." +
	" old_string must be copied VERBATIM from a read_file result, including indentation and line breaks, and must" +
	" match exactly once: if it matches zero times, read the file again and copy the text exactly; if it matches" +
	" several times, include more surrounding lines to make it unique, or pass replace_all: true. new_string replaces" +
	" it — pass an empty string to delete. A near-miss old_string (whitespace or a line or two off) may still match" +
	" via the forgiving fallback; when it does, match_mode in the result says so — re-read the file around" +
	" first_line to confirm the edit landed where you intended. Returns how many replacements were made and the" +
	" line the first one landed on, so you can read back around it. Use write_file instead to create a new file or" +
	" replace one wholesale."

// pathParam is the "a path under the working directory" argument nearly every
// file tool takes; only its prose differs, so the shape is written once.
func pathParam(description string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeString, Description: description}
}

func objectSchema(properties map[string]*genai.Schema, required ...string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeObject, Properties: properties, Required: required}
}

func builtinAgentTools(image string) map[string]builtinTool {
	webFetchDecl, webFetchImpl := webFetchTool(nil)

	return map[string]builtinTool{
		// The catalogue entry is the unrestricted form; a grant carrying
		// allow: is rebuilt with the list bound in (see resolveToolSpec).
		config.WebFetchBuiltinName: {decl: webFetchDecl, impl: webFetchImpl},
		"read_file": {
			decl: &genai.FunctionDeclaration{
				Name:        "read_file",
				Description: toolDescription("read_file", readFileDescription),
				Parameters: objectSchema(map[string]*genai.Schema{
					"path":       pathParam("File path, relative to the working directory (or an absolute path inside it, e.g. one returned by a spilled tool output's pointer message)."),
					"start_line": {Type: genai.TypeInteger, Description: "First line to return, 1-indexed and inclusive. Defaults to 1."},
					"end_line":   {Type: genai.TypeInteger, Description: "Last line to return, 1-indexed and inclusive. Defaults to the end of the file."},
				}, "path"),
			},
			impl: execReadFile,
		},
		"list_dir": {
			decl: &genai.FunctionDeclaration{
				Name:        "list_dir",
				Description: listDirDescription,
				Parameters: objectSchema(map[string]*genai.Schema{
					"path": pathParam("Directory path, relative to the working directory (or an absolute path inside it)."),
				}),
			},
			impl: execListDir,
		},
		"run_shell": {
			decl: &genai.FunctionDeclaration{
				Name:        "run_shell",
				Description: runShellDescription(image),
				Parameters: objectSchema(map[string]*genai.Schema{
					"command": {Type: genai.TypeString, Description: "Command to run via sh -c."},
				}, "command"),
			},
			impl: execRunShell,
		},
		"write_file": {
			decl: &genai.FunctionDeclaration{
				Name:        "write_file",
				Description: toolDescription("write_file", writeFileDescription),
				Parameters: objectSchema(map[string]*genai.Schema{
					"path":    pathParam("File path, relative to the working directory (or an absolute path inside it)."),
					"content": {Type: genai.TypeString, Description: "The text content to write."},
					"append":  {Type: genai.TypeBoolean, Description: "If true, append to the file instead of overwriting it. Defaults to false."},
				}, "path", "content"),
			},
			impl: execWriteFile,
		},
		"edit_file": {
			decl: &genai.FunctionDeclaration{
				Name:        "edit_file",
				Description: toolDescription("edit_file", editFileDescription),
				Parameters: objectSchema(map[string]*genai.Schema{
					"path":        pathParam("File path, relative to the working directory (or an absolute path inside it)."),
					"old_string":  {Type: genai.TypeString, Description: "The exact text to replace, copied verbatim from a read_file result — same indentation, same line breaks."},
					"new_string":  {Type: genai.TypeString, Description: "The text to put in its place. Pass an empty string to delete old_string."},
					"replace_all": {Type: genai.TypeBoolean, Description: "Replace every occurrence instead of requiring exactly one. Defaults to false."},
				}, "path", "old_string", "new_string"),
			},
			impl: execEditFile,
		},
		"search_files": {
			decl: &genai.FunctionDeclaration{
				Name:        "search_files",
				Description: searchFilesDescription,
				Parameters: objectSchema(map[string]*genai.Schema{
					"pattern":          {Type: genai.TypeString, Description: "Regular expression matched against each line of each file. Omit to search by filename only (then glob is required)."},
					"glob":             {Type: genai.TypeString, Description: `Shell pattern a file's path must match, e.g. "**/*.go" or "*_test.go". Omit to search every file.`},
					"path":             pathParam(`Directory to search, relative to the working directory (or an absolute path inside it). Defaults to ".".`),
					"output_mode":      {Type: genai.TypeString, Description: `"files_with_matches" (default), "content", or "count".`, Enum: []string{"files_with_matches", "content", "count"}},
					"head_limit":       {Type: genai.TypeInteger, Description: "Maximum results to return. Clamped to the tool's own ceiling."},
					"case_insensitive": {Type: genai.TypeBoolean, Description: "Match the pattern case-insensitively. Defaults to false."},
				}),
				// Neither pattern nor glob is individually required —
				// execSearchFiles enforces "at least one" instead, which a JSON
				// schema cannot express.
			},
			impl: execSearchFiles,
		},
	}
}
