package agent

// A failure the conversation cannot talk its way out of.

import (
	"errors"
	"sync"
)

// lostTree records the first infrastructure failure a tool hit, so the conversation stops instead of arguing with a machine that is gone.
//
// Every tool failure is ordinarily data: a nonzero exit, a missing file, a refused pattern all come back to the model because the model is who can react to them. A worker that was reclaimed is not that. Handed back as `{"error": ...}` it looks like an ordinary setback, and the model will reasonably try the call again, and again, until max_turns — spending the whole step's budget on a host that no longer exists.
type lostTree struct {
	mu  sync.Mutex
	err error
}

// note records err if it is the kind of failure that ends the step, and ignores it otherwise.
func (l *lostTree) note(err error) {
	if l == nil || err == nil || !errors.Is(err, errNoTreeAccess) {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.err == nil {
		l.err = err
	}
}

// taken is the recorded failure, if there was one.
func (l *lostTree) taken() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.err
}
