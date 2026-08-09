package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

// A stand-in for the `claude` binary, so the CLI-agent path can be exercised
// end to end without a real coding-agent CLI (or a real model, or a network).
//
// It is a shell script on PATH, following internal/shell's writeFakeDocker.
// The one thing to know about writing scripts for it: the driver hands the
// subprocess shell.HostEnv(), an allowlist that carries no FAKE_* variables —
// so a fixture cannot be passed in through the environment the way
// writeFakeDocker passes one. Everything the script needs is baked into its
// text as an absolute path instead.

// fakeCLI is a fake coding-agent CLI installed on PATH, plus the files it
// records what it was asked to do into.
type fakeCLI struct {
	// dir holds one argv-<pid> and one prompt-<pid> file per invocation.
	// Per-invocation rather than one shared log, because concurrent cells
	// (across: with max_in_flight:) run several children at once and two
	// appends to the same file can interleave mid-line.
	dir string
}

// writeFakeClaude installs a fake `claude` on PATH whose body is the given
// shell script. The body writes the CLI's stream-json transcript to stdout;
// argv and stdin capture are already handled around it.
//
// Tests using it must not call t.Parallel: it edits PATH for the process.
func writeFakeClaude(t *testing.T, body string) fakeCLI {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake cli is a shell script")
	}

	dir := t.TempDir()
	records := t.TempDir()
	cli := fakeCLI{dir: records}

	// Arguments separated by "|", newlines flattened: a plain "$*" would not
	// do, since --append-system-prompt carries the multi-line operating note
	// and would otherwise span lines.
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s|' "$@" | tr '\n' ' ' > %[1]q/argv-$$
cat > %[1]q/prompt-$$
%[2]s
`, records, body)

	err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return cli
}

// records returns the contents of every file matching prefix, one per
// invocation, sorted by name so a sequential test reads them in a stable
// order. Concurrent invocations have no meaningful order, so tests over those
// must assert on the SET rather than on positions.
func (c fakeCLI) records(t *testing.T, prefix string) []string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(c.dir, prefix+"-*"))
	if err != nil {
		t.Fatalf("globbing %s records: %v", prefix, err)
	}

	sort.Strings(paths)

	out := make([]string, 0, len(paths))
	for _, path := range paths {
		out = append(out, readFileString(t, path))
	}

	return out
}

// argv returns the argument vector of the nth (1-indexed) invocation.
func (c fakeCLI) argv(t *testing.T, n int) string {
	t.Helper()

	argvs := c.records(t, "argv")
	if n > len(argvs) {
		t.Fatalf("the fake cli ran %d times, wanted invocation %d", len(argvs), n)
	}

	return argvs[n-1]
}

// prompt returns the stdin of the nth (1-indexed) invocation.
func (c fakeCLI) prompt(t *testing.T, n int) string {
	t.Helper()

	prompts := c.records(t, "prompt")
	if n > len(prompts) {
		t.Fatalf("the fake cli ran %d times, wanted invocation %d", len(prompts), n)
	}

	return prompts[n-1]
}

// invocations reports how many times the fake CLI ran.
func (c fakeCLI) invocations(t *testing.T) int {
	t.Helper()

	return len(c.records(t, "argv"))
}

// cliResultEvent is the terminal event of a CLI transcript, as a shell-safe
// single line.
func cliResultEvent(text string, turns int) string {
	return fmt.Sprintf(
		`{"type":"result","subtype":"success","result":%q,"num_turns":%d,"is_error":false,"usage":{"input_tokens":100,"output_tokens":20}}`,
		text, turns)
}

// cliToolUseEvent is an assistant turn calling one tool.
func cliToolUseEvent(id, name, argsJSON string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`, id, name, argsJSON)
}

// callBridgeScript is shell that calls one tool on the steps MCP bridge,
// reading the bridge's URL and bearer token out of the --mcp-config file the
// driver generated. It is how a fake CLI reaches a tool the parent process
// serves — the same round trip the real CLI makes, minus its own model.
//
// Stateless bridge, so this is a single POST: no initialize handshake, no
// session header to carry. The Authorization header is not optional — the
// bridge rejects an unauthenticated caller, which is what stops any other
// local process from running this step's tools.
func callBridgeScript(tool, argsJSON string) string {
	return fmt.Sprintf(`
config=$(echo "$*" | tr ' ' '\n' | grep -A0 'steps-cli-mcp' | head -1)
url=$(sed 's/.*"url":"\([^"]*\)".*/\1/' "$config")
token=$(sed 's/.*"Authorization":"Bearer \([^"]*\)".*/\1/' "$config")
curl -sS -X POST "$url" \
  -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}' >/dev/null
`, tool, argsJSON)
}

// cliErrorResultEvent is the terminal event of a run the CLI itself judged a
// failure. The real binary EXITS NONZERO when it emits one (verified: a
// max-turns stop exits 1), so the fake does too — a fake that exited 0 here
// would let a regression test pass on behavior the real CLI does not have.
func cliErrorResultEvent(subtype, message string) string {
	return fmt.Sprintf(
		`{"type":"result","subtype":%q,"result":"","num_turns":8,"is_error":true,"errors":[%q]}`,
		subtype, message)
}
