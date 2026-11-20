package workspace

// Stable working paths for host steps (#259 2d): a step that runs on this machine works at one path every run, so a tool keyed on its source directory (Go's build cache) stays warm. Stable when free: a run that finds the path held takes a throwaway one, as caches: does, so timing decides speed and never which files a step sees.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
)

// workDirName holds the stable paths under the root, outside buildDirPrefix so the stale-build sweep leaves the locks of running steps alone.
const workDirName = "steps-work"

const workKeyDomain = "steps/work-dir/v1"

// StableWorkKey names a host step's stable path: pipeline, job and step name, as a task cache is scoped.
func StableWorkKey(pipeline, job, step string) string {
	sum := sha256.New()

	for _, part := range []string{workKeyDomain, pipeline, job, step} {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}

	return hex.EncodeToString(sum.Sum(nil))
}

// HostTaskSpace is TaskSpace for a step that runs on this machine, at key's stable path when no other run holds it.
func (b *isolatingBuild) HostTaskSpace(ctx context.Context, key, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, error) {
	dir, release, ok := b.claimWorkDir(ctx, key)
	if !ok {
		return b.newSpace(ctx, label, inputs, outputs, inputMapping, outputMapping)
	}

	return b.spaceIn(ctx, dir, release, inputs, outputs, inputMapping, outputMapping, nil, false)
}

// claimWorkDir takes key's stable path, emptied of whatever an earlier holder left (a crash skips its Close). Not under --keep-workspace, whose step directories stay in the build tree to be read; and not where the path cannot be locked, since an unlocked one could be shared.
func (b *isolatingBuild) claimWorkDir(ctx context.Context, key string) (string, func(), bool) {
	if b.workRoot == "" || b.keep || key == "" {
		return "", nil, false
	}

	err := os.MkdirAll(b.workRoot, 0o750)
	if err != nil {
		slog.DebugContext(ctx, "workspace.work_dir_unavailable", "error", err)

		return "", nil, false
	}

	release, err := tryLock(filepath.Join(b.workRoot, key+".lock"))
	if err != nil {
		slog.DebugContext(ctx, "workspace.work_dir_busy", "key", key, "error", err)

		return "", nil, false
	}

	dir := filepath.Join(b.workRoot, key)

	err = b.backend.remove(dir)
	if err == nil {
		err = os.Mkdir(dir, 0o750)
	}

	if err != nil {
		release()
		slog.WarnContext(ctx, "workspace.work_dir_unavailable", "dir", dir, "error", err)

		return "", nil, false
	}

	return dir, release, true
}
