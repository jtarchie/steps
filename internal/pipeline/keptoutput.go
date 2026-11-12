package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/jtarchie/steps/internal/store"
)

// keptBatchBytes is how much of an elided output waits in memory before it is appended to the store: one row per batch, not per chunk.
const keptBatchBytes = 256 << 10

type keptOutputsKey struct{}

// keptOutputs is one run's way to store an elided output whole, and how many each of its steps has stored so far.
type keptOutputs struct {
	st    store.Events
	runID string
	mu    sync.Mutex
	next  map[int64]int
}

func withKeptOutputs(ctx context.Context, st store.Events, runID string) context.Context {
	return context.WithValue(ctx, keptOutputsKey{}, &keptOutputs{st: st, runID: runID, next: map[int64]int{}})
}

func keptOutputsFrom(ctx context.Context) *keptOutputs {
	kept, _ := ctx.Value(keptOutputsKey{}).(*keptOutputs)

	return kept
}

// claim is the ordinal of the step's next stored output; attempts: records one output per attempt, so a step has several.
func (k *keptOutputs) claim(stepID int64) int {
	k.mu.Lock()
	defer k.mu.Unlock()

	n := k.next[stepID]
	k.next[stepID] = n + 1

	return n
}

// keptOutput is one output being appended to the store, bounded at store.MaxStepOutputBytes.
type keptOutput struct {
	*keptOutputs
	stepID  int64
	n       int
	seq     int
	pending []byte
	taken   int64
	dropped int64
	// failed stops an output the store refused from being offered as whole.
	failed bool
}

func (k *keptOutput) write(ctx context.Context, p []byte) {
	// A refused output is never flushed again, so buffering more of it would only grow.
	if k.failed {
		return
	}

	room := max(store.MaxStepOutputBytes-k.taken, 0)
	take := min(int64(len(p)), room)
	k.dropped += int64(len(p)) - take
	k.taken += take
	k.pending = append(k.pending, p[:take]...)

	if len(k.pending) >= keptBatchBytes {
		k.flush(ctx)
	}
}

func (k *keptOutput) flush(ctx context.Context) {
	if len(k.pending) == 0 || k.failed {
		return
	}

	// Under its own context: the step being cancelled is the likeliest time its output is wanted.
	err := k.st.AppendStepOutput(context.WithoutCancel(ctx), k.runID, k.stepID, k.n, k.seq, k.pending)
	if err != nil {
		// ponytail: pieces already stored stay until the run is reaped; nothing links them, so only a hand-typed URL reads the partial output.
		slog.WarnContext(ctx, "kept output", "step", k.stepID, "error", err)

		k.failed = true
	}

	k.seq++
	k.pending = k.pending[:0]
}

// finish stores the rest, saying what the bound dropped, and reports whether the whole is there.
func (k *keptOutput) finish(ctx context.Context) bool {
	if k.dropped > 0 {
		k.pending = fmt.Appendf(k.pending, "\n... [%d further bytes dropped: a kept output stops at %d bytes]\n", k.dropped, store.MaxStepOutputBytes)
	}

	k.flush(ctx)

	return !k.failed
}

// detail is what the step_output event carries so the run page can name the stored output.
func (k *keptOutput) detail() string { return strconv.Itoa(k.n) }
