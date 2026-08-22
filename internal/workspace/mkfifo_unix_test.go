//go:build unix

package workspace

import (
	"syscall"
	"testing"
)

// mkfifo creates a named pipe: an entry digestTree records the presence of but
// whose content no archive can carry, which is what the codec has to refuse.
func mkfifo(t *testing.T, path string) {
	t.Helper()

	err := syscall.Mkfifo(path, 0o600)
	if err != nil {
		t.Skipf("mkfifo is unavailable here: %v", err)
	}
}
