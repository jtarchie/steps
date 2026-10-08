package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Memories is what an agent step keeps about one scope — a person, usually —
// from one run to the next: short facts a model chose to remember, handed to
// the next run that names the same scope.
//
// The scope is the caller's, never the model's: every method takes it, and
// none reaches across it, which is what keeps one user's memory away from
// another's when a model is talked into asking.
type Memories interface {
	// Remember files text under memory.Scope as its newest entry, then evicts
	// the oldest past limit (zero means no limit; a negative limit caps at
	// DefaultMemoryEntries). Text the scope already holds is not filed twice:
	// the entry already there comes back, reporting false.
	Remember(ctx context.Context, memory Memory, limit int) (Memory, bool, error)
	// ListMemories is scope's entries, newest first, capped by limit (zero
	// means no limit).
	ListMemories(ctx context.Context, scope string, limit int) ([]Memory, error)
	// Forget deletes one entry of scope. An id that is another scope's reports
	// false and deletes nothing — the id comes from a model.
	Forget(ctx context.Context, scope string, id int64) (bool, error)
	// ForgetScope deletes every entry of scope, reporting how many went.
	ForgetScope(ctx context.Context, scope string) (int, error)
	// MemoryScopes is every scope holding an entry, by name.
	MemoryScopes(ctx context.Context) ([]MemoryScope, error)
}

// Memory is one remembered fact.
type Memory struct {
	ID    int64
	Scope string
	Text  string
	// RunID is the run that remembered it, "" once run_history: has reaped
	// that run — the memory outlives it, which is the point of keeping one.
	RunID     string
	CreatedAt string
}

// MemoryScope is one scope's summary, for listing what is kept without
// reading all of it.
type MemoryScope struct {
	Scope   string
	Entries int
	// LastAt is when its newest entry was remembered.
	LastAt string
}

// DefaultMemoryEntries bounds a scope's entries when nothing says otherwise.
// A count, like every other cap here, and the oldest go first. Override with
// defaults.memory_entries: in the pipeline.
const DefaultMemoryEntries = 100

// MaxMemoryBytes caps one entry. A memory is a fact, not a transcript: one
// that does not fit is a model saving the conversation, which is the next
// run's preload filling up with one entry.
const MaxMemoryBytes = 1024

// MaxMemoryScopeBytes caps a scope's name. A scope is an id — a Slack user id
// is eleven bytes — and one this long is a file that was not one.
const MaxMemoryScopeBytes = 256

// ErrMemoryRefused is a memory no driver will file: empty, or over a cap.
var ErrMemoryRefused = errors.New("memory refused")

// CheckMemory is what every driver refuses before writing, so the refusal is
// the contract's rather than one driver's.
func CheckMemory(memory Memory) error {
	err := CheckMemoryScope(memory.Scope)
	if err != nil {
		return err
	}

	switch {
	case !storable(memory.Text):
		return fmt.Errorf("%w: it is not UTF-8 text without NUL bytes", ErrMemoryRefused)
	case memory.Text == "":
		return fmt.Errorf("%w: the text is empty", ErrMemoryRefused)
	case len(memory.Text) > MaxMemoryBytes:
		return fmt.Errorf("%w: the text is %d bytes, over the %d one memory may be", ErrMemoryRefused, len(memory.Text), MaxMemoryBytes)
	}

	return nil
}

// CheckMemoryScope is the scope half of CheckMemory, for a reader to refuse
// before it lists: Postgres cannot even compare against a scope it could not
// store, so a scope only sqlite accepted would fail one driver's step and not
// the other's.
func CheckMemoryScope(scope string) error {
	switch {
	case scope == "":
		return fmt.Errorf("%w: the scope is empty", ErrMemoryRefused)
	case len(scope) > MaxMemoryScopeBytes:
		return fmt.Errorf("%w: the scope is %d bytes, over the %d a scope may be", ErrMemoryRefused, len(scope), MaxMemoryScopeBytes)
	case !storable(scope):
		// Refused rather than cleaned: Postgres stores neither a NUL nor
		// invalid UTF-8 in text, and a driver that quietly repaired them would
		// file different text from the other driver, and dedupe it differently.
		return fmt.Errorf("%w: the scope is not UTF-8 text without NUL bytes", ErrMemoryRefused)
	}

	return nil
}

func storable(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}
