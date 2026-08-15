package exprlang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCheckSeesSourceAndVersion(t *testing.T) {
	t.Parallel()

	in := Input{
		Source:  map[string]any{"channels": []any{"a", "b"}},
		Version: map[string]any{"ts": "100"},
	}

	versions, err := RunCheck(context.Background(),
		`source.channels | map(({channel: #, ts: version.ts}))`, in)
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if len(versions) != 2 || versions[1]["channel"] != "b" || versions[0]["ts"] != "100" {
		t.Fatalf("versions = %+v", versions)
	}
}

// TestRunCheckFirstPollHasEmptyVersion is the ergonomic the whole cursor
// design turns on: on the first-ever check there is nothing recorded, and
// `version.ts ?? "0"` has to be both the natural spelling and the correct
// one. A nil map would make it a panic or a type error instead.
func TestRunCheckFirstPollHasEmptyVersion(t *testing.T) {
	t.Parallel()

	versions, err := RunCheck(context.Background(), `[{since: version.ts ?? "0"}]`, Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if len(versions) != 1 || versions[0]["since"] != "0" {
		t.Fatalf("versions = %+v, want the fallback", versions)
	}
}

func TestRunCheckRejectsNonArray(t *testing.T) {
	t.Parallel()

	_, err := RunCheck(context.Background(), `{ref: "v1"}`, Input{})
	if err == nil || !strings.Contains(err.Error(), "want an array of version objects") {
		t.Fatalf("err = %v, want a message naming the wanted shape", err)
	}

	_, err = RunCheck(context.Background(), `["v1"]`, Input{})
	if err == nil || !strings.Contains(err.Error(), "want an object") {
		t.Fatalf("err = %v, want a message about the element shape", err)
	}
}

func TestRunInReturnsFileMap(t *testing.T) {
	t.Parallel()

	in := Input{
		Version: map[string]any{"id": "42"},
		Params:  map[string]any{"lines": "5"},
	}

	files, err := RunIn(context.Background(),
		`{"version.json": toJSON(version), "lines.txt": params.lines}`, in)
	if err != nil {
		t.Fatalf("RunIn: %v", err)
	}

	// toJSON indents. Valid JSON either way, but worth pinning so nobody
	// writes a test (or a doc example) expecting a compact object.
	if files["version.json"] != "{\n  \"id\": \"42\"\n}" || files["lines.txt"] != "5" {
		t.Fatalf("files = %+v", files)
	}
}

// TestRunInRejectsNonStringValue keeps a mistyped expression from silently
// writing a file full of Go formatting. The error names toJSON because that
// is the fix nine times out of ten.
func TestRunInRejectsNonStringValue(t *testing.T) {
	t.Parallel()

	_, err := RunIn(context.Background(), `{"thread.json": version}`, Input{Version: map[string]any{"a": "b"}})
	if err == nil || !strings.Contains(err.Error(), "toJSON()") {
		t.Fatalf("err = %v, want a message pointing at toJSON()", err)
	}
}

func TestRunOutReturnsVersionOrNil(t *testing.T) {
	t.Parallel()

	version, err := RunOut(context.Background(), `{channel: params.channel}`,
		Input{Params: map[string]any{"channel": "C1"}})
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	if version["channel"] != "C1" {
		t.Fatalf("version = %+v", version)
	}

	// A put that publishes without producing a version is a real shape — the
	// shell backend already tolerates an out: that prints nothing.
	version, err = RunOut(context.Background(), `nil`, Input{})
	if err != nil {
		t.Fatalf("RunOut(nil): %v", err)
	}

	if version != nil {
		t.Fatalf("version = %+v, want nil", version)
	}
}

// TestSlotScopeIsEnforced pins that a slot sees only what it has. An out
// expression reaching for `version` is a real confusion — a put produces a
// version, it does not consume one — and catching it at compile time is
// better than rendering it as nothing.
func TestSlotScopeIsEnforced(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		slot Slot
		src  string
	}{
		"check has no params": {SlotCheck, `[{a: params.x}]`},
		"out has no version":  {SlotOut, `{a: version.x}`},
		"check has no file()": {SlotCheck, `[{a: file("x")}]`},
		"in has no file()":    {SlotIn, `{"a": file("x")}`},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := Compile(test.slot, test.src)
			if err == nil {
				t.Fatalf("Compile(%s, %q): want an error", test.slot, test.src)
			}
		})
	}
}

// TestDateBuiltinsStrippedFromCheckAndIn covers the hazard that would
// otherwise be found in production: a check calling now() returns a
// different "version" every poll, and a version is supposed to be a stable
// identity, so the pipeline re-runs forever with whatever an agent step
// costs attached.
func TestDateBuiltinsStrippedFromCheckAndIn(t *testing.T) {
	t.Parallel()

	for _, slot := range []Slot{SlotCheck, SlotIn} {
		for _, fn := range []string{`now()`, `date("2026-01-01")`, `duration("1h")`} {
			err := Compile(slot, `[{a: string(`+fn+`)}]`)
			if err == nil {
				t.Errorf("Compile(%s, %s): want an error — a version must not depend on the clock", slot, fn)
			}
		}
	}

	// out may still use them: a published message can carry a timestamp, and
	// a shell out: can already call date.
	err := Compile(SlotOut, `{at: string(now())}`)
	if err != nil {
		t.Errorf("Compile(out, now()): %v, want it allowed", err)
	}
}

func TestCompileReportsSyntaxErrors(t *testing.T) {
	t.Parallel()

	err := Compile(SlotCheck, `source.channels | map(`)
	if err == nil || !strings.Contains(err.Error(), "expr.check") {
		t.Fatalf("err = %v, want an error naming the slot", err)
	}
}

func TestEnvFunc(t *testing.T) {
	// No t.Parallel: t.Setenv mutates the process environment.
	t.Setenv("STEPS_TEST_TOKEN", "s3cret")

	in := Input{EnvAllow: []string{"STEPS_TEST_TOKEN", "STEPS_TEST_UNSET"}}

	versions, err := RunCheck(context.Background(), `[{token: env("STEPS_TEST_TOKEN")}]`, in)
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if versions[0]["token"] != "s3cret" {
		t.Fatalf("token = %v", versions[0]["token"])
	}

	// Not declared in env: — the allowlist is the whole list, so this is a
	// pipeline saying it wanted a variable it never asked for.
	_, err = RunCheck(context.Background(), `[{token: env("HOME")}]`, in)
	if err == nil || !strings.Contains(err.Error(), "env:") {
		t.Fatalf("err = %v, want the error to name the env: list", err)
	}

	// Declared but unset is an error, not "": an unset token would otherwise
	// send an unauthenticated request that fails later and as something else.
	_, err = RunCheck(context.Background(), `[{token: env("STEPS_TEST_UNSET")}]`, in)
	if err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("err = %v, want an error about the variable being unset", err)
	}

	// ...unless a default says otherwise.
	versions, err = RunCheck(context.Background(), `[{token: env("STEPS_TEST_UNSET", "anon")}]`, in)
	if err != nil {
		t.Fatalf("RunCheck (default): %v", err)
	}

	if versions[0]["token"] != "anon" {
		t.Fatalf("token = %v, want the default", versions[0]["token"])
	}
}

func TestFileFunc(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "reply.md"), []byte("hello"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(dir, "thread"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "thread", "ts"), []byte("123\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	in := Input{Dir: dir}

	version, err := RunOut(context.Background(),
		`{text: file("reply.md"), ts: trim(file("thread/ts"))}`, in)
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	if version["text"] != "hello" || version["ts"] != "123" {
		t.Fatalf("version = %+v", version)
	}

	// A missing file is an error unless a default is given.
	_, err = RunOut(context.Background(), `{a: file("nope.txt")}`, in)
	if err == nil {
		t.Fatal("RunOut(missing file): want an error")
	}

	version, err = RunOut(context.Background(), `{a: file("nope.txt", "")}`, in)
	if err != nil || version["a"] != "" {
		t.Fatalf("version = %+v, err = %v, want the default", version, err)
	}
}

// TestFileFuncPathGuard: file() reads the put's inputs and nothing else. The
// interesting cases are the ones a prefix check gets wrong — a path that
// climbs out through a directory it first descends into, and an absolute
// path that ignores the base entirely.
func TestFileFuncPathGuard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	secret := filepath.Join(filepath.Dir(dir), "secret.txt")

	err := os.WriteFile(secret, []byte("nope"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Remove(secret) })

	for _, path := range []string{"../secret.txt", "a/../../secret.txt", secret, "/etc/passwd", ""} {
		_, err := RunOut(context.Background(), `{a: file(`+quote(path)+`)}`, Input{Dir: dir})
		if err == nil {
			t.Errorf("file(%q): want an error, the path escapes the put's inputs", path)

			continue
		}

		if !strings.Contains(err.Error(), "relative path inside") {
			t.Errorf("file(%q): err = %v, want the guard's message", path, err)
		}
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// TestFailFunc covers the only way an expression can refuse. It matters most
// for out: a JSON API that answers 200 with {"ok": false} would otherwise
// leave a failed publish looking like a put that produced no version — green,
// and having done nothing.
func TestFailFunc(t *testing.T) {
	t.Parallel()

	_, err := RunOut(context.Background(),
		`false ? {a: "b"} : fail("slack: not_in_channel")`, Input{})
	if err == nil || !strings.Contains(err.Error(), "not_in_channel") {
		t.Fatalf("err = %v, want the message the expression named", err)
	}

	// The ternary short-circuits, so the happy path never evaluates it.
	version, err := RunOut(context.Background(),
		`true ? {a: "b"} : fail("unreachable")`, Input{})
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	if version["a"] != "b" {
		t.Fatalf("version = %+v", version)
	}

	// Available to check too, where "the API said no" is equally real.
	_, err = RunCheck(context.Background(), `fail("nope")`, Input{})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want the failure", err)
	}
}
