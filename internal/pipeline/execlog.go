package pipeline

import (
	"context"
	"fmt"
	"slices"
)

// execLog is the ordered list of entity names that actually ran during one
// RunJob invocation — task/agent/put/get names and the names of any hook
// steps — in execution order. It backs a job's assert.execution self-check.
// Recording happens only at the pipeline dispatch points (never inside
// internal/agent), so the log lives entirely in this package.
type execLog struct {
	names []string
}

func (l *execLog) record(name string) {
	l.names = append(l.names, name)
}

func (l *execLog) snapshot() []string {
	out := make([]string, len(l.names))
	copy(out, l.names)

	return out
}

type execLogKeyType struct{}

// execLogKey is the context key under which RunJob stashes the per-invocation
// execLog. Unexported and package-local, so nothing outside pipeline can read
// or write it.
var execLogKey = execLogKeyType{} //nolint:gochecknoglobals // zero-size context key sentinel

// withExecLog returns ctx carrying log, so the dispatch points reached through
// it can record without any signature changes.
func withExecLog(ctx context.Context, log *execLog) context.Context {
	return context.WithValue(ctx, execLogKey, log)
}

// recordExecution appends name to the execLog carried by ctx, if any. It is a
// no-op when ctx carries no log (every caller outside a RunJob assert context —
// trigger, unit tests — is unaffected).
func recordExecution(ctx context.Context, name string) {
	log, ok := ctx.Value(execLogKey).(*execLog)
	if !ok {
		return
	}

	log.record(name)
}

// checkExecution compares the names that actually ran (got) against the
// ordered names an assert.execution requires (want), returning nil on an exact
// match and an error naming the ordered diff otherwise.
func checkExecution(label string, want, got []string) error {
	if slices.Equal(want, got) {
		return nil
	}

	return fmt.Errorf("%s: assert.execution mismatch:\n  want: %v\n  got:  %v", label, want, got)
}
