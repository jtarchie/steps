package pipeline

// What a restarted steps needs to pick a reattachable command back up (steps#306 phase 2): how much of its output was shown, whether its run is still live, and whether this process is going down with the run left to resume.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
)

// withResumption installs the run's store behind every reattachable command under ctx.
func withResumption(ctx context.Context, st store.Store, opts Run) context.Context {
	return shell.WithResumption(ctx, shell.Resumption{
		Shown: func(ctx context.Context, container string) (int64, int64, bool) {
			return commandShown(ctx, st, container)
		},
		// Through the bus, behind the chunks it counts, so the sink stores the count after the output it covers: a restart neither repeats nor loses a line.
		Save: func(ctx context.Context, container string, stdout, stderr int64) {
			publishStepText(ctx, events.TypeCommandOffset, "", fmt.Sprintf("%d %d", stdout, stderr), container)
		},
		Leave: func() bool { return opts.Interrupted != nil && opts.Interrupted() },
		// A run this store does not know (another pipeline's, another database's) is spared: removing a command its own resume would adopt costs that command, sparing one costs a container.
		Live: func(ctx context.Context, runID string) bool {
			row, found, err := st.FindRunRow(ctx, runID)

			return err != nil || !found || row.Status == store.RunRunning
		},
	})
}

// commandShown is the last count recorded for container in this run.
func commandShown(ctx context.Context, st store.Events, container string) (int64, int64, bool) {
	rows, err := st.RunEvents(ctx, runIDFrom(ctx), 0, 0)
	if err != nil {
		slog.WarnContext(ctx, "run.command_offset_unread", "container", container, "error", err)

		return 0, 0, false
	}

	var (
		stdout, stderr int64
		found          bool
	)

	for _, row := range rows {
		if row.Type != events.TypeCommandOffset || row.Detail != container {
			continue
		}

		_, err = fmt.Sscanf(row.Text, "%d %d", &stdout, &stderr)
		found = err == nil
	}

	return stdout, stderr, found
}
