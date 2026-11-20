package pipeline

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/workspace"
)

// shelveTimeout bounds filing a failed run's trees: a digest walk per artifact, which a hung disk must not turn into a run that never ends.
const shelveTimeout = 10 * time.Minute

type failedBuildsKey struct{}

// failedBuilds are the triggered builds of a run that failed, kept for a resume until the run's outcome decides whether to file them.
type failedBuilds struct {
	mu     sync.Mutex
	builds []workspace.BuildWorkspace
}

func withFailedBuilds(ctx context.Context) (context.Context, *failedBuilds) {
	failed := &failedBuilds{}

	return context.WithValue(ctx, failedBuildsKey{}, failed), failed
}

func noteFailedBuild(ctx context.Context, bw workspace.BuildWorkspace) {
	failed, _ := ctx.Value(failedBuildsKey{}).(*failedBuilds)
	if failed == nil {
		return
	}

	failed.mu.Lock()
	failed.builds = append(failed.builds, bw)
	failed.mu.Unlock()
}

// shelve files each failed build's artifacts into the store under its own context, since the run's may be what ended it; a build that cannot be filed whole keeps the rest in its tree, which a resume still reads.
func (f *failedBuilds) shelve(ctx context.Context, job workspace.BuildWorkspace) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shelveTimeout)
	defer cancel()

	f.mu.Lock()
	builds := append(slices.Clone(f.builds), job)
	f.mu.Unlock()

	// A resumed run's job build and triggered build continue in one tree; the triggered one knows every hold, so it alone files that tree.
	filed := map[string]bool{}

	for _, bw := range builds {
		if filed[bw.Root()] {
			continue
		}

		filed[bw.Root()] = true

		err := bw.Shelve(ctx)
		if err != nil {
			slog.WarnContext(ctx, "workspace.shelve_failed", "dir", bw.Root(), "error", err)
		}
	}
}
