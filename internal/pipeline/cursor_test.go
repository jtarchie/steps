package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// refusingMarks files a version's order but loses the job's mark over it.
type refusingMarks struct{ store.Store }

func (refusingMarks) RecordConsumedMark(context.Context, string, string, int64) error {
	return os.ErrPermission
}

// Taking a version is best-effort, so a lost mark is said: it is the only trace of why the version is built again on the next trigger.
func TestALostCursorMarkIsWarned(t *testing.T) {
	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cursor := &versionCursor{marks: map[string]int64{}, orders: map[string]map[store.VersionKey]int64{"repo": {}}}
	cursor.take(t.Context(), refusingMarks{st}, "build", "repo", map[string]any{"ref": "abc"})

	if !strings.Contains(buf.String(), "job.cursor_unrecorded") {
		t.Errorf("no warning for the lost mark; log:\n%s", buf.String())
	}
}
