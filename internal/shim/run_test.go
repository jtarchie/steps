package shim

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestMainAnswersTheBootstrapArgv pins that cmd/steps-shim takes the exact
// argv the aws:// bootstrap sends a `steps _shim` (ssm.go), so either binary
// can be the one ?shim= or ?binary= names.
func TestMainAnswersTheBootstrapArgv(t *testing.T) {
	var stdout bytes.Buffer

	err := Main([]string{"_shim", "--listen", "127.0.0.1:0", "--once", "--linger", "50ms"}, strings.NewReader(""), &stdout)
	if err != nil {
		t.Fatalf("Main: %v", err)
	}

	if !strings.HasPrefix(stdout.String(), "listening on 127.0.0.1:") {
		t.Errorf("stdout = %q, want the bound address the bootstrap greps for", stdout.String())
	}
}

func TestMainRefusesAnotherVerb(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{nil, {"run"}, {"_shim", "--nope"}, {"_shim", "extra"}} {
		err := Main(args, strings.NewReader(""), &bytes.Buffer{})
		if !errors.Is(err, errNotShim) {
			t.Errorf("Main(%q) = %v, want the usage error", args, err)
		}
	}
}
