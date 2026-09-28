package pipeline

import (
	"context"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/jtarchie/steps/internal/shim"
)

// TestMain answers as the shim because a local: worker execs this test binary as `_shim`; goleak because across, ensemble and parallel fan work out on goroutines.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "_shim" {
		serveShim()
	}

	// A registry bypassed by mistake acquires through the real EC2 client, and that must fail here rather than reach whatever account the shell has. Process-wide rather than a t.Setenv per test, because t.Setenv forbids t.Parallel.
	err := os.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	if err != nil {
		panic(err)
	}

	goleak.VerifyTestMain(m)
}

func serveShim() {
	build, err := shim.SelfBuild()
	if err != nil {
		os.Exit(1)
	}

	err = shim.Serve(context.Background(), os.Stdin, os.Stdout, shim.Options{Build: build})
	if err != nil {
		os.Exit(1)
	}

	os.Exit(0)
}
