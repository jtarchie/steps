package agent

// The one file a containerized agent still reads from this machine.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreachableTree stands in for a container this process cannot reach, so any call that goes to it is visible as a failure rather than as a quietly correct answer. Its root is spelled like a worker's, which is also what makes a host path fail to resolve against it.
type unreachableTree struct{ calls []string }

var errStubUnreachable = errors.New("the stub container was asked for something")

func (u *unreachableTree) note(op string) error {
	u.calls = append(u.calls, op)

	return errStubUnreachable
}

func (u *unreachableTree) root() string { return "/worker/steps/01-hand" }

func (u *unreachableTree) resolve(_ context.Context, rel string) (string, error) {
	err := u.note("resolve " + rel)

	return "", err
}

func (u *unreachableTree) resolveWrite(_ context.Context, rel string) (string, error) {
	err := u.note("resolveWrite " + rel)

	return "", err
}

func (u *unreachableTree) stat(context.Context, string) (treeStat, error) {
	return treeStat{}, u.note("stat")
}

func (u *unreachableTree) readBytes(context.Context, string, int64) ([]byte, error) {
	return nil, u.note("readBytes")
}

func (u *unreachableTree) openRange(context.Context, string, int) (io.ReadCloser, error) {
	return nil, u.note("openRange")
}

func (u *unreachableTree) writeFile(context.Context, string, []byte, bool) error {
	return u.note("writeFile")
}

func (u *unreachableTree) listDir(context.Context, string) ([]treeEntry, error) {
	return nil, u.note("listDir")
}

func (u *unreachableTree) search(context.Context, string, searchOpts) (searchResult, error) {
	return searchResult{}, u.note("search")
}

// spillEnv is a containerized agent whose spill directory holds one file, as it does after an oversized run_shell output was captured here.
func spillEnv(t *testing.T) (toolEnv, *unreachableTree, string) {
	t.Helper()

	dir := t.TempDir()
	spillDir := filepath.Join(dir, toolOutputSpillDirName)

	err := os.MkdirAll(spillDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	spilled := filepath.Join(spillDir, "out-1.txt")

	err = os.WriteFile(spilled, []byte("the overflowing output\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	stub := &unreachableTree{}

	return toolEnv{dir: dir, spillDir: spillDir, tree: stub}, stub, spilled
}

// TestReadFileReadsTheSpillDirectoryHere is the exception working. The model is handed an absolute path by shell.SpillPointerMessage and reads it back on its next turn; routed into the container that path names nothing, because the capture happened on this side and the file was never part of the step's tree.
func TestReadFileReadsTheSpillDirectoryHere(t *testing.T) {
	t.Parallel()

	env, stub, spilled := spillEnv(t)

	got := execReadFile(t.Context(), map[string]any{"path": spilled}, env)

	if errMsg, bad := got["error"]; bad {
		t.Fatalf("read_file of a spilled output failed: %v", errMsg)
	}

	if content, _ := got["content"].(string); !strings.Contains(content, "the overflowing output") {
		t.Errorf("content = %q, want the spilled file's text", content)
	}

	if len(stub.calls) != 0 {
		t.Errorf("the container was asked %v; a spill read must never leave this machine", stub.calls)
	}
}

// TestSpillIsTheOnlyException holds the rule to being exactly one rule. Anything the model can WRITE has to land where run_shell can see it, and a search or a listing that answered from here would be describing a tree the shell is not working in — so every other tool goes to the container even for a path under the spill directory.
func TestSpillIsTheOnlyException(t *testing.T) {
	t.Parallel()

	cases := map[string]func(context.Context, map[string]any, toolEnv) map[string]any{
		"write_file":   execWriteFile,
		"edit_file":    execEditFile,
		"list_dir":     execListDir,
		"search_files": execSearchFiles,
	}

	args := map[string]map[string]any{
		"write_file":   {"content": "x"},
		"edit_file":    {"old_string": "a", "new_string": "b"},
		"list_dir":     {},
		"search_files": {"pattern": "x"},
	}

	for name, exec := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, stub, spilled := spillEnv(t)

			call := map[string]any{"path": spilled}
			for k, v := range args[name] {
				call[k] = v
			}

			if name == "list_dir" || name == "search_files" {
				call["path"] = filepath.Dir(spilled)
			}

			got := exec(t.Context(), call, env)

			if _, bad := got["error"]; !bad {
				t.Fatalf("%s answered from this machine for a spill path: %v", name, got)
			}

			if len(stub.calls) == 0 {
				t.Errorf("%s never reached the container; only read_file may stay here", name)
			}
		})
	}
}

// TestReadFileStillRefusesOtherHostPaths keeps the exception from becoming a hole: a path that merely happens to be absolute, or to sit beside the spill directory, is still the container's to answer.
func TestReadFileStillRefusesOtherHostPaths(t *testing.T) {
	t.Parallel()

	env, stub, spilled := spillEnv(t)

	beside := filepath.Join(env.dir, "not-spilled.txt")

	err := os.WriteFile(beside, []byte("host only\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// Made to EXIST here, which is what gives the assertion teeth: a read wrongly kept on this machine would succeed and hand the model host content, rather than merely failing for some other reason.
	err = os.MkdirAll(filepath.Join(env.dir, "data"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(env.dir, "data", "AGENT.txt"), []byte("host copy\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// The relative path matters most: it is what a model writes nearly every time, and it has to reach the container or a routed agent would read this machine for all its ordinary work.
	for _, path := range []string{"data/AGENT.txt", beside, "/etc/passwd", filepath.Dir(spilled) + "/../escape.txt"} {
		t.Run(path, func(t *testing.T) {
			before := len(stub.calls)

			got := execReadFile(t.Context(), map[string]any{"path": path}, env)
			if _, bad := got["error"]; !bad {
				t.Fatalf("read_file(%q) answered from this machine: %v", path, got)
			}

			// The error alone proves nothing — a host read of a missing file fails too. What the container was ASKED is the only thing that distinguishes the two.
			if len(stub.calls) == before {
				t.Errorf("read_file(%q) never reached the container; the exception is matching more than the spill directory", path)
			}
		})
	}
}
