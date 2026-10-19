package venue

// Reaping the task caches a pipeline's current config no longer names, what Concourse's set-pipeline does to its task_caches rows.

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/shell"
)

const (
	taskCacheLabel         = "steps.task-cache"
	taskCachePipelineLabel = "steps.task-cache.pipeline"
)

type liveCachesKey struct{}

type liveCaches struct {
	pipeline string
	keys     []string
}

// WithLiveTaskCaches names every cache pipeline's current config declares (workspace.TaskCacheKey); a worker a step under ctx connects to drops that pipeline's others. Concourse reaps when the pipeline is set; a worker is only reachable when a step is placed on it, so here is when. A run of an older config still in flight can reap a cache the newer one just made, which costs that cache one cold run.
func WithLiveTaskCaches(ctx context.Context, pipeline string, keys []string) context.Context {
	return context.WithValue(ctx, liveCachesKey{}, liveCaches{pipeline: pipeline, keys: keys})
}

// liveTaskCachesFrom is false with nothing to judge by: no pipeline means no scope, and reaping without one would reach every pipeline's caches.
func liveTaskCachesFrom(ctx context.Context) (liveCaches, bool) {
	live, ok := ctx.Value(liveCachesKey{}).(liveCaches)

	return live, ok && live.pipeline != ""
}

// reapTaskCaches removes this pipeline's cache volumes its config no longer names. Best effort: a cache left behind only costs disk.
func (s *plusSession) reapTaskCaches(ctx context.Context) {
	live, ok := liveTaskCachesFrom(ctx)
	if !ok || !reapDue(s.worker.String()+"\x00"+s.namespace, live) {
		return
	}

	volumes, err := s.docker.ListVolumes(ctx, map[string]string{taskCachePipelineLabel: live.pipeline})
	if err != nil {
		slog.WarnContext(ctx, "venue.docker.task_cache_reap_failed", "worker", s.worker.String(), "error", err)

		return
	}

	for _, volume := range volumes {
		key := volume.Labels[taskCacheLabel]
		// A name that is not this namespace's for its key is another local: worker's cache on the same daemon.
		if slices.Contains(live.keys, key) || volume.Name != taskCacheVolume(s.namespace, key) {
			continue
		}

		// Refused while mounted, which is what keeps a step still running an older config whole.
		err = s.docker.RemoveVolume(ctx, volume.Name)
		if err == nil {
			slog.InfoContext(ctx, "venue.docker.task_cache_reaped", "worker", s.worker.String(), "volume", volume.Name)
		}
	}
}

// reaped remembers which worker has been reaped against which config, so a job's every step does not pay a daemon round trip (over a tunnel, for a cloud worker) for an answer that only a new config changes.
var reaped sync.Map //nolint:gochecknoglobals // per-process memory, like swept

// reapDue is true the first time this process reaps worker for this pipeline's set of live caches. An orphan left by a run of an older config still in flight waits for the next config, which costs only disk.
func reapDue(worker string, live liveCaches) bool {
	keys := slices.Sorted(slices.Values(live.keys))
	_, done := reaped.LoadOrStore(worker+"\x00"+live.pipeline+"\x00"+strings.Join(keys, " "), true)

	return !done
}

func taskCacheLabels(ctx context.Context, key string) map[string]string {
	labels := shell.OwnershipLabels()
	labels[taskCacheLabel] = key

	if pipeline := shell.MetadataFrom(ctx).PipelineName; pipeline != "" {
		labels[taskCachePipelineLabel] = pipeline
	}

	return labels
}

// reapScript is the shell that removes this pipeline's kept cache directories its config no longer names, judged by the .pipeline file each one is filed with; empty with nothing to judge by.
func reapScript(ctx context.Context, root string) string {
	live, ok := liveTaskCachesFrom(ctx)
	if !ok {
		return ""
	}

	// The keys are hex digests, so the space-joined list needs no quoting inside the case word.
	return `c=` + root + `/steps-task-caches; for f in "$c"/*.pipeline; do [ -f "$f" ] && [ "$(cat "$f")" = ` + shellQuote(live.pipeline) +
		` ] || continue; k=${f##*/}; k=${k%.pipeline}; case " ` + strings.Join(live.keys, " ") + ` " in *" $k "*) ;; *) rm -rf "$c/$k" "$f" ;; esac; done; `
}
