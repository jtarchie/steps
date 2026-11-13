package shell

// Telling a venue that a command cannot have changed anything.

import "context"

// readOnlyKey marks a context whose command only reads.
type readOnlyKey struct{}

// ReadOnly marks ctx as carrying a command that cannot modify the tree, so a venue can skip bringing the outputs home afterwards.
//
// The fetch after every command is what keeps a placed step's assert: and its local tree honest, and for a task — one command, then the check — it costs one transfer. An agent is different in kind: a conversation issues dozens of file-tool calls, and a read_file that fetched the step's outputs each time would pay a full transfer, over a worker's tunnel or as a presigned PUT, to bring back a tree nothing had touched.
//
// It is stated by the caller rather than inferred from the command, because only the caller knows: a shell string cannot be read for whether it writes, and guessing wrong in the permissive direction loses a model's edits.
func ReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, true)
}

// IsReadOnly reports whether ctx was marked by ReadOnly.
func IsReadOnly(ctx context.Context) bool {
	marked, _ := ctx.Value(readOnlyKey{}).(bool)

	return marked
}

// deferFetchKey marks a context whose commands leave the outputs on the worker until Sync.
type deferFetchKey struct{}

// DeferFetch marks ctx so a venue keeps a write's outputs where they were made until Sync: an agent writes dozens of times, and bringing a repository home after each one cost a full transfer per edit (steps#246).
func DeferFetch(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferFetchKey{}, true)
}

// IsFetchDeferred reports whether ctx was marked by DeferFetch.
func IsFetchDeferred(ctx context.Context) bool {
	marked, _ := ctx.Value(deferFetchKey{}).(bool)

	return marked
}

// Syncer is a runner whose outputs can lag this machine's copy of the tree.
type Syncer interface {
	Sync(ctx context.Context) error
}

// Sync brings home whatever runner deferred; a runner that never defers has nothing to bring.
func Sync(ctx context.Context, runner Runner) error {
	syncer, ok := runner.(Syncer)
	if !ok {
		return nil
	}

	return syncer.Sync(ctx) //nolint:wrapcheck // the venue's error names what failed to arrive
}
