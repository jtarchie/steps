package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

type pausedFails struct {
	store.Store
	err error
}

func (p pausedFails) Paused(context.Context) (bool, error) { return false, p.err }

// Not t.Parallel(): captureLogs swaps the process's logger. A shutdown cancels the drain mid-read, which is the daemon stopping, not a problem worth an ERR on a terminal that is slog only; a read that fails on a live drain still is one.
func TestAPauseReadCutByShutdownIsNotLoggedAsAProblem(t *testing.T) {
	logs := captureLogs(t)

	_, target, st := drainable(t, t.TempDir(), "jobs:\n  - name: build\n    plan:\n      - task: say\n        run: echo hi\n")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	target.Store = pausedFails{Store: st, err: context.Canceled}

	if admits(ctx, target) {
		t.Error("a drain whose pause read was cancelled admitted a claim")
	}

	if strings.Contains(logs.String(), "web.paused") {
		t.Errorf("a shutdown's cancelled pause read was logged:\n%s", logs)
	}

	target.Store = pausedFails{Store: st, err: errors.New("database is locked")}

	if admits(t.Context(), target) {
		t.Error("a pause read that failed admitted a claim")
	}

	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "database is locked") {
		t.Errorf("a pause read that failed on a live drain was not logged as an error:\n%s", logs)
	}
}
