package agent

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

const secretContent = "outside the working directory"

// escapeFixture builds root/{work,outside} where work holds every kind of link a model's run_shell could have planted before calling a file tool.
func escapeFixture(t testing.TB) (root, work string) {
	t.Helper()

	root, err := os.MkdirTemp(t.TempDir(), "fuzz")
	if err != nil {
		t.Fatal(err)
	}

	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	work = filepath.Join(root, "work")
	outside := filepath.Join(root, "outside")

	for _, d := range []string{filepath.Join(work, "sub"), outside} {
		err = os.MkdirAll(d, 0o750)
		if err != nil {
			t.Fatal(err)
		}
	}

	for p, content := range map[string]string{
		filepath.Join(outside, "secret"): secretContent,
		filepath.Join(work, "sub", "f"):  "inside",
	} {
		err = os.WriteFile(p, []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	for name, target := range map[string]string{
		"in":      "sub",
		"out":     "../outside",
		"abs":     outside,
		"leak":    "../outside/secret",
		"dangle":  "../outside/new",
		"dangdir": "../outside/newdir/x",
		"loop":    "loop",
		"self":    ".",
	} {
		err = os.Symlink(target, filepath.Join(work, name))
		if err != nil {
			t.Fatal(err)
		}
	}

	return root, work
}

// outsideUntouched fails when anything under root other than work differs from what escapeFixture made.
func outsideUntouched(t *testing.T, root, rel string) {
	t.Helper()

	err := filepath.WalkDir(filepath.Join(root, "outside"), func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		switch filepath.Base(p) {
		case "outside":
			return nil
		case "secret":
			data, readErr := os.ReadFile(p) //nolint:gosec // p is under the root this test made
			if readErr != nil || string(data) != secretContent {
				t.Fatalf("write through %q changed the file outside the working directory: %q, %v", rel, data, readErr)
			}

			return nil
		default:
			t.Fatalf("write through %q created %s outside the working directory", rel, p)

			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Fatalf("write through %q left %d entries beside work and outside", rel, len(entries))
	}
}

func pathSeeds(f *testing.F) {
	for _, seed := range []string{
		"", ".", "sub/f", "in/f", "in/new", "new/deep/file",
		"out", "out/secret", "out/new", "secret", "dangle", "dangdir", "loop", "loop/x", "self/sub/f", "self/../outside/secret", "leak",
		"abs/secret", "abs/new", "../outside/secret", "sub/../../outside/secret", "$ROOT/outside/new", "$ROOT/work/sub/f",
		"$ROOT/work/../outside/new", "/etc/passwd", "sub/\x00", "in/../out/new",
	} {
		f.Add(seed)
	}
}

// FuzzResolveAgentPath: whatever read_file/list_dir is allowed to open, it opens inside the working directory once every link is followed.
func FuzzResolveAgentPath(f *testing.F) {
	pathSeeds(f)

	root, work := escapeFixture(f)

	f.Fuzz(func(t *testing.T, rel string) {
		rel = strings.ReplaceAll(rel, "$ROOT", root)

		resolved, err := resolveAgentPath(work, rel)
		if err != nil {
			return
		}

		if !within(work, resolved) {
			t.Fatalf("resolveAgentPath(%q) = %q, lexically outside %q", rel, resolved, work)
		}

		_, statErr := os.Stat(resolved)
		if statErr != nil {
			return
		}

		target, err := filepath.EvalSymlinks(resolved)
		if err != nil {
			t.Fatalf("resolveAgentPath(%q) = %q, which stats but does not resolve: %v", rel, resolved, err)
		}

		if !within(work, target) {
			t.Fatalf("resolveAgentPath(%q) = %q, which opens %q outside %q", rel, resolved, target, work)
		}
	})
}

// FuzzResolveWritePath performs the write write_file would, then proves nothing outside the working directory changed.
func FuzzResolveWritePath(f *testing.F) {
	pathSeeds(f)

	f.Fuzz(func(t *testing.T, rel string) {
		root, work := escapeFixture(t)
		rel = strings.ReplaceAll(rel, "$ROOT", root)

		resolved, err := resolveWritePath(work, rel)
		if err != nil {
			return
		}

		if !within(work, resolved) {
			t.Fatalf("resolveWritePath(%q) = %q, lexically outside %q", rel, resolved, work)
		}

		_ = hostTree{dir: work}.writeFile(context.Background(), resolved, []byte("written"), true)

		outsideUntouched(t, root, rel)
	})
}

// FuzzLexicalResolve holds the container half to the same confinement, with no filesystem to lean on.
func FuzzLexicalResolve(f *testing.F) {
	for _, seed := range [][2]string{
		{"/work", "a/b"}, {"/work", "../x"}, {"/work", "/work/../x"}, {"/work/", "a"}, {"/", "etc"},
		{"/work", "/workspace/x"}, {"/work", "a/../../work2"}, {"/work", ""}, {"/w", "//w/x"},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, dir, rel string) {
		if !path.IsAbs(dir) {
			return
		}

		resolved, err := lexicalResolve(dir, rel)
		if err != nil {
			if !path.IsAbs(rel) && !hasDotDot(rel) && path.Clean(dir) != "/" {
				t.Fatalf("lexicalResolve(%q, %q) refused a relative path with no ..: %v", dir, rel, err)
			}

			return
		}

		checkConfined(t, dir, rel, resolved)
	})
}

func checkConfined(t *testing.T, dir, rel, resolved string) {
	t.Helper()

	base := path.Clean(dir)
	if resolved != base && !strings.HasPrefix(resolved, base+"/") {
		t.Fatalf("lexicalResolve(%q, %q) = %q, outside %q", dir, rel, resolved, base)
	}

	if path.Clean(resolved) != resolved || hasDotDot(strings.TrimPrefix(resolved, base)) {
		t.Fatalf("lexicalResolve(%q, %q) = %q, not a clean path", dir, rel, resolved)
	}
}

// within is the fuzzers' oracle for containment, spelled out here rather than borrowed from config.ConfinedPath, the code under test.
func within(base, p string) bool {
	return p == base || strings.HasPrefix(p, base+string(os.PathSeparator))
}

func hasDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}

	return false
}
