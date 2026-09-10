package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/web"
)

// A daemon whose loops did not share machines would let one job's end stop the instance another is on — the registry off in the one mode it exists for, with nothing else able to notice.
func TestTheDaemonSharesAcquiredMachinesAcrossItsLoops(t *testing.T) {
	t.Parallel()

	held := newDaemon(t.Context(), nil, web.NewLocalRunner(nil, nil, 1, false),
		filepath.Join(t.TempDir(), "steps.db"), ExecFlags{}, HistoryFlags{}, time.Hour)
	defer held.Close()

	if !pipeline.SharesWorkers(held.base) {
		t.Fatal("the daemon's loops run on a context with no shared registry")
	}
}
