package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/venue"
)

type refusingPlacements struct{ store.Store }

func (refusingPlacements) RecordPlacement(context.Context, store.Placement) error {
	return os.ErrPermission
}

// Where a step ran is recorded best-effort, so a lost row is said rather than leaving a machine nobody can name.
func TestALostPlacementIsWarned(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, sink := withPlacementSink(events.WithRunID(t.Context(), "run-1"))
	sink.note(venue.Placement{Tag: "box"})

	recordPlacement(ctx, stepRunner{jobName: "build", st: refusingPlacements{}}, sink, 0, "work", "", "")

	if !strings.Contains(buf.String(), "job.placement_unrecorded") {
		t.Errorf("no warning for the lost placement; log:\n%s", buf.String())
	}
}
