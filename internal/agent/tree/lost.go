package tree

// A failure the conversation cannot talk its way out of.

import (
	"errors"
	"sync"
)

// Lost records the first infrastructure failure a tool hit, so the conversation stops instead of arguing with a machine that is gone.
//
// Every tool failure is ordinarily data: a nonzero exit, a missing file, a refused pattern all come back to the model because the model is who can react to them. A worker that was reclaimed is not that. Handed back as `{"error": ...}` it looks like an ordinary setback, and the model will reasonably try the call again, and again, until max_turns — spending the whole step's budget on a host that no longer exists.
type Lost struct {
	mu  sync.Mutex
	err error
}

// Note records err if it is the kind of failure that ends the step, and ignores it otherwise.
func (l *Lost) Note(err error) {
	if l == nil || err == nil || !errors.Is(err, ErrNoAccess) {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.err == nil {
		l.err = err
	}
}

// Taken is the recorded failure, if there was one.
func (l *Lost) Taken() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.err
}
