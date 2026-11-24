package shell

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/testdocker"
)

// dash, Debian's and Ubuntu's sh, reads `kill -KILL -- -"$p"` as an illegal number and kills nothing, so a retry raced the command it replaced (steps#306).
func TestReapOrphansKillsTheGroupUnderDash(t *testing.T) {
	testdocker.Require(t)

	runner, err := NewRunner(RunnerSpec{Image: "debian:stable-slim"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	seconds := fmt.Sprintf("3%07d", os.Getpid()%10_000_000)
	// No ps in a slim image; /proc says what runs.
	count := `for p in /proc/[0-9]*; do tr '\0' ' ' < "$p/cmdline" 2>/dev/null; echo; done | grep -c 'slee[p] ` + seconds + `'`

	deadline, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	_, _, code, err := RunCaptureFull(deadline, runner, "sleep "+seconds)
	if err != nil || code != -1 {
		t.Fatalf("the cut-off command: code %d, err %v; want it reported as signalled", code, err)
	}

	stdout, _, _, err := RunCaptureFull(t.Context(), runner, count)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}

	if got := strings.TrimSpace(stdout); got != "0" {
		t.Errorf("%s copies of the cut-off command still run under dash; the reap killed nothing", got)
	}
}
