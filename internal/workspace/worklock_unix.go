//go:build darwin || linux

package workspace

import (
	"fmt"
	"os"
	"syscall"
)

// tryLock holds path's flock until the returned func runs. flock rather than a marker file: the kernel drops it when the holder dies, so a crashed run never keeps a stable path busy, and two opens in one process conflict as two processes do.
func tryLock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // a key this package derived, under its own root
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("locking %s: %w", path, err)
	}

	return func() { _ = file.Close() }, nil
}
