package trigger

// Where this package's user-facing lines go.

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// out is stdout, behind a lock, so a test can read what a watch printed
// without racing the watch that is printing it.
//
// It exists because the test helper that captured this output assigned the
// os.Stdout GLOBAL while the package's other tests — parallel, and most of
// them printing — read the same variable. That is a genuine data race, not a
// load flake: -race reported it, intermittently, on a package whose tests are
// otherwise deterministic, and an intermittently red gate is one people learn
// to re-run rather than read.
//
// A read lock per line is invisible here: these are human-facing lines emitted
// a handful of times per poll, never in a loop over work.
//
//nolint:gochecknoglobals // one process-wide destination for one process's own output
var (
	outMu sync.RWMutex
	out   io.Writer = os.Stdout
)

// printf writes one line to wherever this package's output currently goes.
//
// Errors are dropped, as fmt.Printf drops them: a watch that cannot write to
// its own stdout has nothing useful to do about it, and nothing above here
// would act on the answer either.
func printf(format string, args ...any) {
	outMu.RLock()
	defer outMu.RUnlock()

	_, _ = fmt.Fprintf(out, format, args...)
}
