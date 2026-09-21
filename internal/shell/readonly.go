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
