package store

// The single-watcher lock.

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// AcquireWatchLock takes an exclusive, non-blocking file lock next to the
// database, marking this process as THE watcher. held reports that another
// live process has it; release must be called when the watch ends.
//
// It exists because watch startup is destructive to other watchers:
// ResetStaleRunning assumes any 'running' queue row is an abandoned leftover
// and flips it back to pending — true at single-process startup, false when
// another watch is mid-build, where it means the same job claimed twice and
// serial:/max_in_flight silently defeated. That assumption became easy to
// violate the moment `steps watch --once` was pitched for cron: a build
// outliving the cron interval makes overlap the NORMAL case, not an error.
//
// An OS lock rather than a row in the database, because the failure being
// guarded against is a process DYING without cleanup — a row would then say
// "a watcher is alive" forever, while flock evaporates with the process
// holding it. The blast radius of the reverse failure (NFS and friends where
// flock is advisory-at-best) is refusing to start, which is loud.
//
// `steps web` and `steps run` do not take it: neither resets queue rows, and
// serializing them against the watcher is SQLite's job, not this lock's.
func (s *Store) AcquireWatchLock() (release func(), held bool, err error) {
	lockPath := s.path + ".watch.lock"

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // sibling of the db path the caller chose
	if err != nil {
		return nil, false, fmt.Errorf("could not open watch lock %q: %w", lockPath, err)
	}

	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = file.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}

		return nil, false, fmt.Errorf("could not lock %q: %w", lockPath, err)
	}

	return func() {
		// Closing releases the flock; the file itself stays, as lock files
		// do — its existence means nothing, only the lock does.
		_ = file.Close()
	}, false, nil
}
