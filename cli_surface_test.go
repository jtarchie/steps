package main

// The shape of the command line itself: which verbs exist, and whether a flag
// a command DECLARES is one it actually applies.
//
// The second half is not a hypothetical. --version-history was declared on
// `run` and threaded nowhere, so it silently did nothing there and worked
// only under `watch` — a flag that reads as configured while binding nothing.
// Sharing a flag between commands via an embed makes that failure cheaper to
// repeat (one declaration, many commands, each of which must still apply it),
// so the embeds arrive with the tests that would catch it.

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestTopLevelCommandsAreTheDocumentedSet pins the CLI's verb list.
//
// A consolidation that means to remove commands should have to say so here,
// in one place, rather than being counted by hand from a help dump — and a
// command added by accident (an embed pulling in a struct with a cmd tag, a
// copy-paste) is otherwise invisible until somebody reads the help.
func TestTopLevelCommandsAreTheDocumentedSet(t *testing.T) {
	t.Parallel()

	want := map[string]bool{
		"Run": true, "Watch": true, "Test": true, "Validate": true,
		"Runs": true, "Plan": true, "MCP": true, "Preflight": true,
		"Jobs": true, "Approvals": true, "Approve": true, "Questions": true,
		"Answer": true, "Reject": true, "Web": true, "Docs": true,
	}

	got := map[string]bool{}
	cli := reflect.TypeOf(CLI{})

	for index := range cli.NumField() {
		field := cli.Field(index)

		_, isCmd := field.Tag.Lookup("cmd")
		if !isCmd {
			continue
		}

		// _shim is hidden on purpose: it is the remote half of a placed step,
		// not a verb anybody types.
		if _, hidden := field.Tag.Lookup("hidden"); hidden {
			continue
		}

		got[field.Name] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("command %s is gone; update this list if that was deliberate", name)
		}
	}

	for name := range got {
		if !want[name] {
			t.Errorf("command %s appeared without being listed here", name)
		}
	}
}

// flagFixture writes a pipeline that any command can be pointed at.
func flagFixture(t *testing.T) string {
	t.Helper()

	return writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: "true"
`)
}

// TestWorkerFlagAppliesWhereverItIsDeclared.
//
// --worker is declared by four commands and parsed in one place, which is the
// arrangement that lets a command declare it and never call the parser. An
// unparseable worker URL is the cheapest proof that each of them does: the
// command must refuse before it does any work, and a command that never
// applied the flag would sail past it.
func TestWorkerFlagAppliesWhereverItIsDeclared(t *testing.T) {
	t.Parallel()

	path := flagFixture(t)

	for _, args := range [][]string{
		{"run", path, "--job", "build"},
		{"watch", path, "--once"},
		{"test", path},
		// Port 1 is unbindable as an ordinary user, so a regression here
		// fails in a second rather than serving until the test binary's own
		// timeout — which is how this case behaved when it was first
		// sabotaged to check it worked.
		{"web", path, "--listen", "127.0.0.1:1"},
	} {
		t.Run(args[0], func(t *testing.T) {
			err := run(append(args, "--worker", "gpu=nope://x"))
			if err == nil {
				t.Fatalf("%s accepted an unparseable --worker, so it never applied the flag", args[0])
			}

			if !strings.Contains(err.Error(), "worker") {
				t.Errorf("%s failed for some other reason: %v", args[0], err)
			}
		})
	}
}

// TestVarsFileAppliesWhereverItIsDeclared is the same proof for the other
// widely shared pair: seven commands declare --var/--vars-file, and all seven
// load the pipeline through the one loader that reads it.
func TestVarsFileAppliesWhereverItIsDeclared(t *testing.T) {
	t.Parallel()

	path := flagFixture(t)
	missing := filepath.Join(t.TempDir(), "absent.yml")

	for _, args := range [][]string{
		{"run", path, "--job", "build"},
		{"watch", path, "--once"},
		{"test", path},
		{"validate", path},
		{"plan", path, "--job", "build"},
		{"preflight", path, "--job", "build"},
		{"web", path},
	} {
		t.Run(args[0], func(t *testing.T) {
			err := run(append(args, "--vars-file", missing))
			if err == nil {
				t.Fatalf("%s accepted a --vars-file that does not exist, so it never read one", args[0])
			}

			if !strings.Contains(err.Error(), "vars file") {
				t.Errorf("%s failed for some other reason: %v", args[0], err)
			}
		})
	}
}
