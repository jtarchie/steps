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
	"strings"
	"syscall"

	"github.com/jtarchie/steps/internal/config"
)

// taskCacheDirName sits beside the step cache under the root, outside buildDirPrefix so sweepStaleBuilds spares it.
const taskCacheDirName = "steps-task-caches"

// defaultTaskCacheMaxEntries counts caches, not bytes, like every other store under the root; one entry is one step's one path.
const defaultTaskCacheMaxEntries = 50

const taskCacheDomain = "steps/task-cache/v1"

// rename is a test seam: an EXDEV is only real across filesystems, which a unit test cannot make.
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
	type attached struct{ entry, dir, rel string }

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

		if !symlinkFree(dir, rel) {
			slog.WarnContext(ctx, "workspace.task_cache_symlinked", "dir", dst)

			continue
		}

		attach(ctx, entry, dst, ok)

		if ok {
			done = append(done, attached{entry: entry, dir: dir, rel: rel})
		}
	}

	// Detached: a cancelled step is still one whose cache is worth filing back, and a copy across filesystems runs cp under this context.
	saveCtx := context.WithoutCancel(ctx)

	return func() {
		for _, a := range done {
			b.detach(saveCtx, a.entry, a.dir, a.rel)
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
		err = move(ctx, entry, dst)
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

// symlinkFree reports that no directory between dir and dir/rel is a symlink. An input keeps its symlinks and a step can plant one, so without this a cache's remove or rename would follow one out of dir: deleting a host directory, or filing it into a cache the next container mounts.
func symlinkFree(dir, rel string) bool {
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	cur := dir

	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)

		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return true
		}

		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return false
		}
	}

	return true
}

// detach files dir/rel back as the entry, replacing one a concurrent run filed first. An empty cache is not kept: the next attach makes an empty directory anyway, and keeping it would spend an LRU slot on nothing.
func (b *isolatingBuild) detach(ctx context.Context, entry, dir, rel string) {
	dst := filepath.Join(dir, rel)

	if !symlinkFree(dir, rel) {
		slog.WarnContext(ctx, "workspace.task_cache_symlinked", "dir", dst)

		return
	}

	entries, err := os.ReadDir(dst)
	if err != nil {
		return
	}

	if len(entries) == 0 {
		_ = os.Remove(dst)

		return
	}

	err = b.fileBack(ctx, entry, dst)
	// A concurrent run can file the entry between the remove and the rename, which then refuses; one retry keeps the last finisher's.
	if err != nil {
		err = b.fileBack(ctx, entry, dst)
	}

	if err != nil {
		slog.WarnContext(ctx, "workspace.task_cache_save_failed", "dir", dst, "error", err)

		return
	}

	b.taskCaches.touch(entry)
}

func (b *isolatingBuild) fileBack(ctx context.Context, entry, dst string) error {
	err := b.taskCaches.removeEntry(entry)
	if err != nil {
		return fmt.Errorf("removing the kept cache: %w", err)
	}

	return move(ctx, dst, entry)
}

// move renames, or copies and removes when the two sides are different filesystems. Only EXDEV copies: any other refusal, a concurrent run's entry already in place among them, copied would merge two caches into one.
func move(ctx context.Context, src, dst string) error {
	err := rename(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	err = os.MkdirAll(dst, 0o750)
	if err == nil {
		err = copyTree(ctx, src, dst)
	}

	if err != nil {
		_ = os.RemoveAll(dst)

		return fmt.Errorf("copying the cache across filesystems: %w", err)
	}

	err = os.RemoveAll(src)
	if err != nil {
		return fmt.Errorf("removing the moved cache: %w", err)
	}

	return nil
}

// ClearTaskCaches deletes the caches keyed by keys under ws's root, reporting which were there; ws must name a root, since under a temp one no cache outlives its run.
func ClearTaskCaches(ws *config.WorkspaceConfig, keys []string) (map[string]bool, error) {
	provider, err := NewProvider(ws, false)
	if err != nil {
		return nil, err
	}

	defer func() { _ = provider.Close() }()

	p, ok := provider.(*isolatingProvider)
	if !ok {
		return nil, fmt.Errorf("%w: provider %T", errUnsupportedStrategy, provider)
	}

	found := make(map[string]bool, len(keys))

	for _, key := range keys {
		entry, usable := p.taskCaches.path(key)
		if !usable {
			continue
		}

		_, statErr := os.Lstat(entry)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}

		err = p.taskCaches.removeEntry(entry)
		if err != nil {
			return found, fmt.Errorf("removing cache %s: %w", entry, err)
		}

		found[key] = true
	}

	return found, nil
}

var errUnsupportedStrategy = errors.New("not supported on this machine")
