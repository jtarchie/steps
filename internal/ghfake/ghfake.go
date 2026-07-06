// Package ghfake installs a stub `gh` on PATH for hermetic tests. It records
// every invocation's argv and returns canned stdout routed by subcommand, so
// tests can exercise the gh.* actions — or a whole workflow that calls them —
// without touching GitHub. The stub IS the mocked GitHub endpoint: set a
// response per subcommand, run the code, then assert on the recorded calls.
package ghfake

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// GH is a handle to an installed fake gh: its temp dir (where response and
// control files live) plus helpers to script responses and assert on calls.
type GH struct {
	Dir      string
	argvPath string
	t        *testing.T
}

// Install writes a stub `gh` onto PATH (prepended) and returns a handle. The
// stub logs each arg on its own line, terminates each invocation with a "--"
// line, and routes stdout: `resp_<cmd>_<sub>` (e.g. resp_pr_diff), falling back
// to `resp_<cmd>` (e.g. resp_api) — so `gh api <path>` is served by resp_api.
// A `fail` file makes it exit with that code (a non-zero exit is a Go error).
func Install(t *testing.T) *GH {
	t.Helper()
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	q := func(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + q(argvPath) + "; done\n" +
		"printf -- '--\\n' >> " + q(argvPath) + "\n" +
		"if [ -f " + q(dir) + "/resp_\"$1\"_\"$2\" ]; then cat " + q(dir) + "/resp_\"$1\"_\"$2\"\n" +
		"elif [ -f " + q(dir) + "/resp_\"$1\" ]; then cat " + q(dir) + "/resp_\"$1\"\n" +
		"fi\n" +
		"if [ -f " + q(dir) + "/fail ]; then echo 'gh: boom' >&2; exit \"$(cat " + q(dir) + "/fail)\"; fi\n" +
		"exit 0\n"
	ghPath := filepath.Join(dir, "gh")
	err := os.WriteFile(ghPath, []byte(script), 0o600)
	if err != nil {
		t.Fatalf("ghfake: writing stub: %v", err)
	}
	err = os.Chmod(ghPath, 0o700)
	if err != nil {
		t.Fatalf("ghfake: chmod stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &GH{Dir: dir, argvPath: argvPath, t: t}
}

// Respond sets the canned stdout for a subcommand key: "pr diff", "pr view",
// "pr comment", "pr edit", "pr review", or "api" (serves every `gh api …`).
func (g *GH) Respond(key, stdout string) {
	g.t.Helper()
	name := "resp_" + strings.ReplaceAll(key, " ", "_")
	err := os.WriteFile(filepath.Join(g.Dir, name), []byte(stdout), 0o600)
	if err != nil {
		g.t.Fatalf("ghfake: Respond(%q): %v", key, err)
	}
}

// Fail makes subsequent invocations exit with code (a non-zero gh exit surfaces
// as a Go action error).
func (g *GH) Fail(code int) {
	g.t.Helper()
	err := os.WriteFile(filepath.Join(g.Dir, "fail"), []byte(strconv.Itoa(code)), 0o600)
	if err != nil {
		g.t.Fatalf("ghfake: Fail: %v", err)
	}
}

// Calls returns every recorded invocation as its argv slice, in order.
func (g *GH) Calls() [][]string {
	g.t.Helper()
	b, err := os.ReadFile(g.argvPath)
	if err != nil {
		return nil
	}
	var calls [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "--" {
			calls = append(calls, cur)
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return calls
}

// FindCall returns the first recorded invocation whose argv contains all the
// given tokens, and whether one was found.
func (g *GH) FindCall(want ...string) ([]string, bool) {
	g.t.Helper()
	for _, call := range g.Calls() {
		set := map[string]bool{}
		for _, a := range call {
			set[a] = true
		}
		ok := true
		for _, w := range want {
			if !set[w] {
				ok = false
				break
			}
		}
		if ok {
			return call, true
		}
	}
	return nil, false
}

// WantCall asserts some recorded invocation contained all the given tokens.
func (g *GH) WantCall(want ...string) {
	g.t.Helper()
	if _, ok := g.FindCall(want...); !ok {
		g.t.Errorf("ghfake: no gh call contained %v; calls: %v", want, g.Calls())
	}
}
