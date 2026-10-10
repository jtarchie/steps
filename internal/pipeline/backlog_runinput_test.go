package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

type failingRunInputs struct {
	store.Versions
	err error
}

func (f failingRunInputs) RecordRunInput(context.Context, string, string, string, string) error {
	return f.err
}

// A lost run-input row is what makes a later resume unable to re-open the version, so losing it must leave a warning behind — and recording it must not.
func TestRecordRunInputWarnsOnlyWhenTheRowIsLost(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	version := map[string]any{"ref": "abc"}

	recordRunInput(context.Background(), failingRunInputs{}, "src", "repo", version)

	if strings.Contains(buf.String(), "job.run_input_unrecorded") {
		t.Errorf("warned about a row that was recorded: %s", buf.String())
	}

	recordRunInput(context.Background(), failingRunInputs{err: errors.New("disk full")}, "src", "repo", version)

	if !strings.Contains(buf.String(), "job.run_input_unrecorded") || !strings.Contains(buf.String(), "disk full") {
		t.Errorf("a lost run-input row left no warning: %q", buf.String())
	}
}
