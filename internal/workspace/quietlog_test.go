package workspace

// Best-effort paths log a failure and carry on, so the log line IS their behaviour: `steps web`'s terminal is slog, and a failure line on success is a false alarm on every build.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r.Clone())

	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// captureSlog swaps the default logger for the rest of the test. Callers are not t.Parallel().
func captureSlog(t *testing.T) *recordingHandler {
	t.Helper()

	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return h
}

func (h *recordingHandler) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.records {
		if r.Message == msg {
			return true
		}
	}

	return false
}

// assertNoFailureLogged fails on any warning or error, and on any line whose error attribute is nil — a failure branch taken on success.
func (h *recordingHandler) assertNoFailureLogged(t *testing.T) {
	t.Helper()

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.records {
		failed := r.Level >= slog.LevelWarn

		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "error" && a.Value.Any() == nil {
				failed = true
			}

			return true
		})

		if failed {
			t.Errorf("a path that succeeded logged %s %q", r.Level, r.Message)
		}
	}
}

type failingClose struct {
	BuildWorkspace
	StepSpace
}

func (failingClose) Close() error { return errors.New("busy") }

func TestCloseHelpersLogOnlyAFailure(t *testing.T) {
	logs := captureSlog(t)

	bw, err := newTestCopyProvider(t).NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatal(err)
	}

	space, err := bw.TaskSpace(ctxT(), "01-work", nil, []string{"out"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	CloseSpace(space, "01-work")
	CloseBuild(bw, "b1")
	logs.assertNoFailureLogged(t)

	CloseSpace(failingClose{}, "02-work")
	CloseBuild(failingClose{}, "b2")

	for _, msg := range []string{"workspace.space_close", "workspace.build_close"} {
		if !logs.has(msg) {
			t.Errorf("a failed Close logged no %s", msg)
		}
	}
}

func TestSweepAndARefusedSpaceLogNoFailure(t *testing.T) {
	logs := captureSlog(t)
	root := t.TempDir()

	err := os.MkdirAll(filepath.Join(root, "b-deadbeef-1-build", "artifacts"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	provider := stepCacheProvider(t, root)

	err = provider.Validate()
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(ctxT(), "b1")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = bw.Close() }()

	_, err = bw.TaskSpace(ctxT(), "01-work", []string{"nonexistent"}, nil, nil, nil)
	if err == nil {
		t.Fatal("TaskSpace with an unmaterialized input should error")
	}

	logs.assertNoFailureLogged(t)
}

func TestResourceCacheLogsNoFailureWhenItWorks(t *testing.T) {
	logs := captureSlog(t)
	bw := cachingBuild(t, t.TempDir(), 1)

	write := func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "V"), []byte("x"), 0o600)
	}

	for _, key := range []string{"key-1", "key-2", "key-2"} {
		_, err := bw.FetchResource(context.Background(), "repo", key, write)
		if err != nil {
			t.Fatal(err)
		}
	}

	if !logs.has("workspace.cache_evict") {
		t.Fatal("nothing was evicted; the scenario does not reach the eviction path")
	}

	logs.assertNoFailureLogged(t)
}

func TestStepCacheMirrorLogsNoFailureWhenItWorks(t *testing.T) {
	logs := captureSlog(t)
	root := t.TempDir()
	blobs := newMemBlobs(t)
	index := newMemIndex()
	req := mirrorRequest()

	runAndStore(t, mirroredBuild(t, root, blobs, index), req)

	err := os.RemoveAll(filepath.Join(root, stepCacheDirName))
	if err != nil {
		t.Fatal(err)
	}

	if !LookupStepCache(context.Background(), mirroredBuild(t, root, blobs, index), req).Hit {
		t.Fatal("a lookup with the bytes only in the blob store missed")
	}

	logs.assertNoFailureLogged(t)
}

func TestDiscardStagedLogsNoFailureWhenItWorks(t *testing.T) {
	logs := captureSlog(t)
	root := t.TempDir()

	cache, err := newStepCache(copyBackend{}, root, 10)
	if err != nil {
		t.Fatal(err)
	}

	leftover := stagingPath(filepath.Join(root, "out"))

	err = os.MkdirAll(leftover, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	cache.discardStaged([]stagedOutput{{tmp: leftover, dst: filepath.Join(root, "out")}})
	logs.assertNoFailureLogged(t)
}
