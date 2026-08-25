package blobstore

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain holds this package to the no-leaked-goroutines rule the rest of
// the repo follows: the SDK's HTTP client pools connections on goroutines,
// and a test that returned while one was still alive would strand it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
