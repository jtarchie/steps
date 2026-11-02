package secret

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain holds the package to the repo's no-leaked-goroutines rule: os/exec copies a source's output on goroutines that must end with the command.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
