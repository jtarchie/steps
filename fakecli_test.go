package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	// argvPath receives the exact argument vector the driver built — the
	// permission boundary, as the child actually saw it.
	argvPath string
	// promptPath receives everything written to the child's stdin.
	promptPath string
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
	cli := fakeCLI{
		argvPath:   filepath.Join(dir, "argv"),
		promptPath: filepath.Join(dir, "prompt"),
	}

	// One line per invocation, arguments separated by "|". A plain "$*" would
	// not do: --append-system-prompt carries the multi-line operating note, so
	// argv is only one line once embedded newlines are flattened.
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s|' "$@" | tr '\n' ' ' >> %[1]q
printf '\n' >> %[1]q
cat >> %[2]q
%[3]s
`, cli.argvPath, cli.promptPath, body)

	err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return cli
}

// argv returns the argument vector of the nth (1-indexed) invocation.
func (c fakeCLI) argv(t *testing.T, n int) string {
	t.Helper()

	lines := strings.Split(strings.TrimRight(readFileString(t, c.argvPath), "\n"), "\n")
	if n > len(lines) {
		t.Fatalf("the fake cli ran %d times, wanted invocation %d", len(lines), n)
	}

	return lines[n-1]
}

// invocations reports how many times the fake CLI ran.
func (c fakeCLI) invocations(t *testing.T) int {
	t.Helper()

	_, err := os.Stat(c.argvPath)
	if err != nil {
		return 0
	}

	return len(strings.Split(strings.TrimRight(readFileString(t, c.argvPath), "\n"), "\n"))
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
