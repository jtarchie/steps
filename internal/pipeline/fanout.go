package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
)

// fanOut runs count items concurrently, bounded by limit, and returns once every admitted item has finished. It owns what in_parallel:, across: and ensemble: share: admission in declaration order, one execution log per item merged back in declaration order, and the recover.
//
// Slots are acquired HERE, in the parent, rather than inside each goroutine. That makes items start in declaration order — under a limit especially, "which two go first" is otherwise whichever goroutines the scheduler happened to run, which is nothing a pipeline author can reason about.
//
// admit runs after the slot is acquired, so its answer reflects the items that have actually finished; false stops admitting, and the items already in flight run to completion. Asking inside the goroutine would instead race every item that had already been admitted.
//
// A panic in run is recovered and handed to onPanic as that item's error, called before the slot is released: a panicking item must not take the daemon down, and it must not leak its slot or strand the items still waiting on one.
func fanOut(
	ctx context.Context, count, limit int,
	admit func(index int) bool,
	run func(ctx context.Context, index int),
	onPanic func(index int, err error),
) {
	var (
		wg   sync.WaitGroup
		slot = newLimiter(limit, count)
		logs = make([]*execLog, count)
	)

	for index := range count {
		slot.acquire()

		if !admit(index) {
			slot.release()

			break
		}

		wg.Add(1)

		// Each item records into its own log, merged back below — items finish in whatever order they finish, and recording as they go would make assert.execution nondeterministic.
		runCtx, itemLog := forkExecLog(ctx)
		logs[index] = itemLog

		go func() {
			defer wg.Done()
			defer slot.release()
			defer func() {
				if p := recover(); p != nil {
					slog.ErrorContext(ctx, "fanout.panic", "index", index, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
					onPanic(index, fmt.Errorf("panic: %v", p))
				}
			}()

			run(runCtx, index)
		}()
	}

	wg.Wait()

	for _, itemLog := range logs {
		if itemLog != nil {
			mergeExecLog(ctx, itemLog)
		}
	}
}
