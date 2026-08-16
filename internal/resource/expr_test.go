package resource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// exprType builds an expr-backed resource type with the given slots.
func exprType(check, in, out string) config.ResourceType {
	return config.ResourceType{
		Name: "api",
		Config: config.ResourceTypeConfig{
			Expr: &config.ExprResourceConfig{Check: check, In: in, Out: out},
		},
	}
}

func TestExprCheckVersionsThroughCheckVersions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"1"},{"id":"2"}]}`))
	}))
	t.Cleanup(server.Close)

	rt := exprType(`http({url: source.url}).json.items | map((
	  {id: #.id}
	))`, "", "")

	versions, err := CheckVersions(context.Background(), nil, rt, nil,
		map[string]any{"url": server.URL}, nil)
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	if len(versions) != 2 || versions[1]["id"] != "2" {
		t.Fatalf("versions = %+v", versions)
	}
}

// TestExprCheckSeesTheCursor: the cursor reaches an expr check as a plain
// value, so the natural spelling (?? on a missing key) is also the correct
// one — no index/default incantation, which is the shell side's tax.
func TestExprCheckSeesTheCursor(t *testing.T) {
	t.Parallel()

	rt := exprType(`[{since: version.ts ?? "0"}]`, "", "")

	versions, err := CheckVersions(context.Background(), nil, rt, nil, nil, map[string]any{"ts": "100"})
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	if versions[0]["since"] != "100" {
		t.Errorf("since = %v, want the cursor", versions[0]["since"])
	}

	versions, err = CheckVersions(context.Background(), nil, rt, nil, nil, nil)
	if err != nil {
		t.Fatalf("CheckVersions (first poll): %v", err)
	}

	if versions[0]["since"] != "0" {
		t.Errorf("since = %v, want the fallback on a first poll", versions[0]["since"])
	}
}

func TestExprRunInWritesFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rt := exprType("", `{
	  "version.json": toJSON(version),
	  "nested/note.txt": params.note,
	}`, "")

	err := RunIn(context.Background(), nil, rt, nil, nil,
		map[string]any{"id": "42"}, map[string]any{"note": "hi"}, dir)
	if err != nil {
		t.Fatalf("RunIn: %v", err)
	}

	var version map[string]any

	data, err := os.ReadFile(filepath.Join(dir, "version.json")) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}

	err = json.Unmarshal(data, &version)
	if err != nil || version["id"] != "42" {
		t.Fatalf("version.json = %s (%v)", data, err)
	}

	// Nested paths are created, not rejected: an artifact is a directory tree.
	note, err := os.ReadFile(filepath.Join(dir, "nested", "note.txt")) //nolint:gosec // t.TempDir
	if err != nil || string(note) != "hi" {
		t.Fatalf("nested/note.txt = %q (%v)", note, err)
	}
}

// TestExprRunInOmittedWritesVersionJSON: detecting that something changed is
// the common case, and it needs no fetch — the same default the mcp backend
// takes.
func TestExprRunInOmittedWritesVersionJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := RunIn(context.Background(), nil, exprType(`[]`, "", ""), nil, nil,
		map[string]any{"id": "7"}, nil, dir)
	if err != nil {
		t.Fatalf("RunIn: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "version.json")) //nolint:gosec // t.TempDir
	if err != nil || !strings.Contains(string(data), `"7"`) {
		t.Fatalf("version.json = %q (%v)", data, err)
	}
}

// TestExprRunInPathGuard: an expression comes from a pipeline file, and a
// pipeline file is not a licence to write anywhere on the machine. The cases
// that matter are the ones a prefix check gets wrong.
func TestExprRunInPathGuard(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"../escaped.txt", "a/../../escaped.txt", "/tmp/escaped.txt"} {
		dir := t.TempDir()

		err := RunIn(context.Background(), nil,
			exprType("", `{`+quoteExpr(path)+`: "x"}`, ""), nil, nil, nil, nil, dir)
		if err == nil {
			t.Errorf("RunIn(%q): want an error, the path escapes the artifact directory", path)

			continue
		}

		if !strings.Contains(err.Error(), "relative path inside") {
			t.Errorf("RunIn(%q): err = %v, want the guard's message", path, err)
		}
	}
}

func TestExprRunOutReadsThePutsInputs(t *testing.T) {
	t.Parallel()

	var got string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		got = string(body)

		_, _ = w.Write([]byte(`{"ts":"999"}`))
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "reply.md"), []byte("hello"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	rt := exprType("", "", `
	  let posted = http({url: source.url, json: {text: file("reply.md")}});
	  {ts: posted.json.ts}
	`)

	version, err := RunOut(context.Background(), nil, rt, nil,
		map[string]any{"url": server.URL}, nil, dir)
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	if version["ts"] != "999" {
		t.Errorf("version = %+v, want the published version echoed back", version)
	}

	if got != `{"text":"hello"}` {
		t.Errorf("posted body = %s, want the file's contents", got)
	}
}

// TestExprRunOutNilVersionTolerated: publishing without producing a version
// is a real shape, and RunOut already tolerates it for shell.
func TestExprRunOutNilVersionTolerated(t *testing.T) {
	t.Parallel()

	version, err := RunOut(context.Background(), nil, exprType("", "", `nil`), nil,
		nil, nil, t.TempDir())
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	if version != nil {
		t.Errorf("version = %+v, want nil", version)
	}
}

// TestCompileExprPrograms is what makes a syntax error a `steps validate`
// error rather than something found on the first poll. It cannot be a load
// error: internal/config imports no expression engine, by design.
func TestCompileExprPrograms(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{ResourceTypes: []config.ResourceType{exprType(`[{a: "b"}]`, "", "")}}

	err := CompileExprPrograms(cfg)
	if err != nil {
		t.Fatalf("CompileExprPrograms: %v", err)
	}

	cfg.ResourceTypes[0].Config.Expr.Check = `source.items | map(`

	err = CompileExprPrograms(cfg)
	if err == nil {
		t.Fatal("CompileExprPrograms: want an error for an unparsable expression")
	}

	if !strings.Contains(err.Error(), `resource_type "api"`) || !strings.Contains(err.Error(), "expr.check") {
		t.Errorf("err = %v, want it to name the resource type and the slot", err)
	}

	// A shell type is not compiled as an expression.
	shell := &config.Config{ResourceTypes: []config.ResourceType{{
		Name:   "sh",
		Config: config.ResourceTypeConfig{Check: "printf '[]'"},
	}}}

	err = CompileExprPrograms(shell)
	if err != nil {
		t.Errorf("CompileExprPrograms(shell): %v, want nil", err)
	}
}

func quoteExpr(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// TestExtraEnvUnionsWithTypeEnv proves CheckVersions' extraEnv parameter
// (config.Resource.Env) widens what env() may read for THIS call, on top of
// the resource type's own env: — without extraEnv, a name the type didn't
// declare is still refused, and a name in neither list is refused even with
// extraEnv supplied, so widening is exact rather than a blanket bypass.
func TestExtraEnvUnionsWithTypeEnv(t *testing.T) {
	t.Setenv("STEPS_TEST_TYPE_TOKEN", "type-value")
	t.Setenv("STEPS_TEST_EXTRA_TOKEN", "extra-value")
	t.Setenv("STEPS_TEST_UNLISTED_TOKEN", "unlisted-value")

	rt := exprType(`[{a: env("STEPS_TEST_TYPE_TOKEN"), b: env("STEPS_TEST_EXTRA_TOKEN")}]`, "", "")
	rt.Env = []string{"STEPS_TEST_TYPE_TOKEN"}

	_, err := CheckVersions(context.Background(), nil, rt, nil, nil, nil)
	if err == nil {
		t.Fatal("CheckVersions with no extraEnv: want an error, STEPS_TEST_EXTRA_TOKEN is not in the type's own env:")
	}

	versions, err := CheckVersions(context.Background(), nil, rt, []string{"STEPS_TEST_EXTRA_TOKEN"}, nil, nil)
	if err != nil {
		t.Fatalf("CheckVersions with extraEnv: %v", err)
	}

	if versions[0]["a"] != "type-value" || versions[0]["b"] != "extra-value" {
		t.Fatalf("versions = %+v, want both the type's own env: and extraEnv readable", versions)
	}

	unlisted := exprType(`[{a: env("STEPS_TEST_UNLISTED_TOKEN")}]`, "", "")
	unlisted.Env = []string{"STEPS_TEST_TYPE_TOKEN"}

	_, err = CheckVersions(context.Background(), nil, unlisted, []string{"STEPS_TEST_EXTRA_TOKEN"}, nil, nil)
	if err == nil {
		t.Fatal("CheckVersions: want an error, STEPS_TEST_UNLISTED_TOKEN is in neither the type's env: nor extraEnv")
	}
}
