//go:build !unix

package workspace

import "testing"

// mkfifo has no portable form off unix; the refusal it exercises is a unix
// concern in the first place.
func mkfifo(t *testing.T, _ string) {
	t.Helper()
	t.Skip("named pipes are a unix concept")
}
