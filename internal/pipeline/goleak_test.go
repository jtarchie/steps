package pipeline

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak because across, ensemble and parallel fan work out on goroutines.
func TestMain(m *testing.M) {
	// A registry bypassed by mistake acquires through the real EC2 client, and that must fail here rather than reach whatever account the shell has. Process-wide rather than a t.Setenv per test, because t.Setenv forbids t.Parallel.
	err := os.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	if err != nil {
		panic(err)
	}

	goleak.VerifyTestMain(m)
}
