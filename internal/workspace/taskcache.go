package workspace

// Task caches: directories a step keeps between runs on this machine (config.TaskCache).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/config"
)

// taskCacheDirName sits beside the step cache under the root, outside buildDirPrefix so sweepStaleBuilds spares it.
const taskCacheDirName = "steps-task-caches"

// defaultTaskCacheMaxEntries counts caches, not bytes, like every other store under the root; one entry is one step's one path.
const defaultTaskCacheMaxEntries = 50

const taskCacheDomain = "steps/task-cache/v1"

// rename is a test seam: an EXDEV is only real across filesystems or btrfs subvolumes, neither of which a unit test can make.
var rename = os.Rename //nolint:gochecknoglobals // test seam

// TaskCacheKey names one cache: Concourse's job + step name + path, under the pipeline that owns the job.
func TaskCacheKey(pipeline, job, step, path string) string {
	sum := sha256.New()

	for _, part := range []string{taskCacheDomain, pipeline, job, step, filepath.Clean(path)} {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}

	return hex.EncodeToString(sum.Sum(nil))
}

// TaskCacheKeys keys every one of a step's caches by its cleaned path, the form AttachCaches takes.
func TaskCacheKeys(pipeline, job, step string, caches []config.TaskCache) map[string]string {
	keyed := make(map[string]string, len(caches))

	for _, path := range config.CachePaths(caches) {
		keyed[path] = TaskCacheKey(pipeline, job, step, path)
	}

	return keyed
}

// AttachCaches moves each kept cache into dir at its path, by rename, and returns what moves them back. A rename rather than a mount or a copy: the cache and every step directory share the root's filesystem, so it costs nothing at any size and works the same for a host command and a bind-mounted container. A concurrent run of the same step finds the entry gone and starts empty (in Concourse both start warm); whichever finishes last is kept, as there.
func (b *isolatingBuild) AttachCaches(ctx context.Context, dir string, caches map[string]string) func() {
	type attached struct{ entry, dst string }

	var done []attached

	for rel, key := range caches {
		var (
			entry string
			ok    bool
		)

		if b.taskCaches != nil {
			entry, ok = b.taskCaches.path(key)
		}

		dst := filepath.Join(dir, rel)

		attach(ctx, entry, dst, ok)

		if ok {
			done = append(done, attached{entry: entry, dst: dst})
		}
	}

	return func() {
		for _, a := range done {
			b.detach(a.entry, a.dst)
		}

		if len(done) > 0 {
			b.taskCaches.prune()
		}
	}
}

// attach leaves dst an existing directory whatever happens: a cache is a speed-up, so failing to restore one is an empty cache, never a failed step.
func attach(ctx context.Context, entry, dst string, ok bool) {
	// A cache inside an input shadows that part of it, as Concourse's mount over it does.
	err := os.RemoveAll(dst)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(dst), 0o750)
	}

	if err == nil && ok {
		err = move(entry, dst)
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}

	if err != nil {
		slog.WarnContext(ctx, "workspace.task_cache_restore_failed", "dir", dst, "error", err)
	}

	err = os.MkdirAll(dst, 0o750)
	if err != nil {
		slog.WarnContext(ctx, "workspace.task_cache_create_failed", "dir", dst, "error", err)
	}
}

// detach files dst back as the entry, replacing one a concurrent run filed first. An empty cache is not kept: the next attach makes an empty directory anyway, and keeping it would spend an LRU slot on nothing.
func (b *isolatingBuild) detach(entry, dst string) {
	entries, err := os.ReadDir(dst)
	if err != nil {
		return
	}

	if len(entries) == 0 {
		_ = os.Remove(dst)

		return
	}

	err = b.taskCaches.removeEntry(entry)
	if err == nil {
		err = move(dst, entry)
	}

	if err != nil {
		slog.Warn("workspace.task_cache_save_failed", "dir", dst, "error", err)

		return
	}

	b.taskCaches.touch(entry)
}

// move renames, or copies and removes when the two sides are different filesystems or btrfs subvolumes (a cache inside an input is inside that input's snapshot). ponytail: a plain copy; a reflink-aware one is the upgrade if a nested cache on btrfs is ever measured to matter.
func move(src, dst string) error {
	err := rename(src, dst)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return err
	}

	copyErr := os.CopyFS(dst, os.DirFS(src))
	if copyErr != nil {
		return errors.Join(err, copyErr)
	}

	err = os.RemoveAll(src)
	if err != nil {
		return fmt.Errorf("removing the moved cache: %w", err)
	}

	return nil
}
