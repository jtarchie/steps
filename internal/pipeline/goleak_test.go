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
