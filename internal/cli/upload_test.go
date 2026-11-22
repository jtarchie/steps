package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/web"
	"github.com/jtarchie/steps/internal/wire"
)

// uploadedSource gets src from a resource that cannot answer, and prints what the upload put there once gate exists.
func uploadedSource(gate string) string {
	return `
resource_types:
- name: unreachable
  config:
    check: exit 1
    in: exit 1
resources:
- name: repo
  type: unreachable
  source: {}
jobs:
- name: build
  plan:
  - get: src
    resource: repo
  - task: work
    inputs: [src]
    run: ` + strconv.Quote("while [ ! -e "+strconv.Quote(gate)+" ]; do sleep 0.02; done; cat src/hello.txt") + "\n"
}

type fakeUploads struct {
	parts []fakePart
	next  int
}

type fakePart struct {
	get     string
	archive []byte
}

func (f *fakeUploads) Next() (string, io.Reader, error) {
	if f.next == len(f.parts) {
		return "", nil, io.EOF
	}

	part := f.parts[f.next]
	f.next++

	return part.get, bytes.NewReader(part.archive), nil
}

// archive packs files of the given sizes as the CLI would.
func archive(t *testing.T, files map[string]int) []byte {
	t.Helper()

	dir := t.TempDir()

	names := make([]string, 0, len(files))

	for name, size := range files {
		err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("x"), size), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		names = append(names, name)
	}

	var packed bytes.Buffer

	err := compress.Pack(&packed, true, func(w io.Writer) error { return wire.PackFiles(w, dir, names) })
	if err != nil {
		t.Fatal(err)
	}

	return packed.Bytes()
}

func uploadRequest(source string, parts ...fakePart) web.OneOffRequest {
	req := web.OneOffRequest{Source: source, Job: "build", Uploads: &fakeUploads{parts: parts}}
	for _, part := range parts {
		req.Inputs = append(req.Inputs, part.get)
	}

	return req
}

// Not t.Parallel(), here and below: a destroy forgets mcp logins in the process environment.
func TestARestartedOneOffStillHasItsUpload(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	gate := filepath.Join(t.TempDir(), "gate")

	created, err := held.OneOff(t.Context(), uploadRequest(uploadedSource(gate), fakePart{"src", archive(t, map[string]int{"hello.txt": 3})}))
	if err != nil {
		t.Fatalf("one-off: %v", err)
	}

	waitRunning(t, held, created.Pipeline)

	sent := served(t, held, created.Pipeline).Inputs["src"]
	interrupted := newestRunOf(t, held, created.Pipeline)

	held.Close()

	restarted := restart(t, held)

	if got := served(t, restarted, created.Pipeline).Inputs["src"]; got != sent || got.Digest == "" {
		t.Fatalf("the restarted one-off's upload = %+v, want %+v as it was sent", got, sent)
	}

	err = os.WriteFile(gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	waitSucceeded(t, restarted, created.Pipeline)

	// The same run, resumed: one built from the top again passes whether or not a resume honors the upload's version.
	if got := newestRunOf(t, restarted, created.Pipeline); got != interrupted {
		t.Errorf("the run that succeeded is %s, want %s resumed", got, interrupted)
	}

	err = restarted.Destroy(t.Context(), created.Pipeline)
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}

	_, err = os.Stat(filepath.Join(restarted.uploadsRoot(), created.Pipeline))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the forgotten one-off's uploads are still on disk: %v", err)
	}
}

func restart(t *testing.T, held *daemon) *daemon {
	t.Helper()

	local := scheduler.NewLocalRunner(nil, pipeline.Run{}, 1)

	server, err := web.New(nil, local)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	restarted := newDaemon(t.Context(), server, local, held.state, held.exec, HistoryFlags{}, time.Hour)
	t.Cleanup(restarted.Close)

	err = restarted.load(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return restarted
}

// waitSucceeded waits for the answer rather than the first finish: an interrupted run reads aborted until its resume finishes it.
func waitSucceeded(t *testing.T, held *daemon, name string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for status := finishedRun(t, held, name); status != "succeeded"; status = finishedRun(t, held, name) {
		if time.Now().After(deadline) {
			t.Fatalf("the resumed one-off's run %s, want succeeded on its upload", status)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// newestRunOf waits for the run's row, because the runner reports a job running before the run has recorded itself: under load the two are a few milliseconds apart.
func newestRunOf(t *testing.T, held *daemon, name string) string {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for {
		runs, err := served(t, held, name).Store.ListRuns(t.Context(), "build", 1)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}

		if len(runs) == 1 {
			return runs[0].ID
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s is running and never recorded a run", name)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

type unreadUploads struct{ t *testing.T }

func (u unreadUploads) Next() (string, io.Reader, error) {
	u.t.Error("an upload was read for a one-off that was refused")

	return "", nil, io.EOF
}

func TestARefusedUploadLeavesNothing(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	held.oneOffInputMax = 100
	source := uploadedSource(filepath.Join(t.TempDir(), "gate"))

	for name, req := range map[string]web.OneOffRequest{
		"a get the job lacks":       {Source: source, Job: "build", Inputs: []string{"nope"}, Uploads: unreadUploads{t}},
		"a folder nobody named":     {Source: source, Job: "build", Inputs: []string{"src"}, Uploads: &fakeUploads{parts: []fakePart{{"other", archive(t, map[string]int{"a": 1})}}}},
		"a named folder never sent": {Source: source, Job: "build", Inputs: []string{"src"}, Uploads: &fakeUploads{}},
		"a folder sent twice": {Source: source, Job: "build", Inputs: []string{"src"}, Uploads: &fakeUploads{parts: []fakePart{
			{"src", archive(t, map[string]int{"a": 1})}, {"src", archive(t, map[string]int{"a": 2})},
		}}},
		"a folder past the bound": uploadRequest(source, fakePart{"src", archive(t, map[string]int{"a": 101})}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := held.OneOff(t.Context(), req)
			if !errors.Is(err, web.ErrRefused) {
				t.Errorf("one-off = %v, want refused", err)
			}
		})
	}

	entries, _ := os.ReadDir(held.uploadsRoot())
	if len(entries) != 0 || len(held.served) != 0 {
		t.Errorf("refused uploads left %d folders and %d one-offs served", len(entries), len(held.served))
	}
}

// The bound is over every folder a one-off carries, not each alone.
func TestTheUploadBoundIsTheOneOffs(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	held.oneOffInputMax = 100

	source := `
resource_types:
- name: unreachable
  config:
    check: exit 1
    in: exit 1
resources:
- name: repo
  type: unreachable
  source: {}
jobs:
- name: build
  plan:
  - get: a
    resource: repo
  - get: b
    resource: repo
`

	_, err := held.OneOff(t.Context(), uploadRequest(source, fakePart{"a", archive(t, map[string]int{"f": 60})}, fakePart{"b", archive(t, map[string]int{"f": 60})}))
	if !errors.Is(err, web.ErrRefused) || !strings.Contains(err.Error(), "more than 100 bytes") {
		t.Errorf("two 60-byte folders under a 100-byte bound = %v, want refused naming the bound", err)
	}

	_, err = held.OneOff(t.Context(), uploadRequest(source, fakePart{"a", archive(t, map[string]int{"f": 40})}, fakePart{"b", archive(t, map[string]int{"f": 40})}))
	if err != nil {
		t.Errorf("two 40-byte folders under a 100-byte bound = %v, want accepted", err)
	}
}

func TestExcludeMatchesAPathItsDirectoriesOrItsName(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		"node_modules/x/index.js": true,
		"a/node_modules/x.js":     false,
		"logs/today.log":          true,
		"build/out.txt":           true,
		"src/main.go":             false,
	} {
		if got := excluded(name, []string{"node_modules", "*.log", "build/*"}); got != want {
			t.Errorf("excluded(%q) = %v, want %v", name, got, want)
		}
	}
}

// fly's listing: a tracked file deleted from the checkout is not sent, an untracked one is, an ignored one is not.
func TestGitFilesAreWhatGitWouldCommit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"kept": "k", "gone": "g", ".gitignore": "ignored\n", "ignored": "i", "new": "n"})

	for _, args := range [][]string{{"init", "-q"}, {"add", "kept", "gone", ".gitignore"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // constant arguments
		cmd.Dir = dir

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	err := os.Remove(filepath.Join(dir, "gone"))
	if err != nil {
		t.Fatal(err)
	}

	files, err := InputFlags{}.files(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{".gitignore", "kept", "new"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}

	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"a/b.txt": ""})

	files, err = InputFlags{}.files(t.Context(), outside)
	if want := []string{"a/b.txt"}; err != nil || !slices.Equal(files, want) {
		t.Errorf("a folder outside git = %v, %v, want everything in it: %v", files, err, want)
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, body := range files {
		path := filepath.Join(dir, name)

		err := os.MkdirAll(filepath.Dir(path), 0o750)
		if err == nil {
			err = os.WriteFile(path, []byte(body), 0o600)
		}

		if err != nil {
			t.Fatal(err)
		}
	}
}
