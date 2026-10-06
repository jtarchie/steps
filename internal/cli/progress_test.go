package cli

import (
	"errors"
	"regexp"
	"testing"

	"golang.org/x/sys/unix"
)

// TestProgressAutoIsPlainOffATerminal: go test's stdout is not a terminal, which is exactly the case auto exists for — a pipe, a CI log, an agent calling steps must get lines, never cursor movement. The explicit modes win either way.
func TestProgressAutoIsPlainOffATerminal(t *testing.T) {
	t.Parallel()

	for mode, want := range map[string]bool{"auto": false, "plain": false, "tty": true} {
		if got := (ProgressFlags{Progress: mode}).wantsLive(); got != want {
			t.Errorf("--progress=%s live = %v, want %v", mode, got, want)
		}
	}
}

// NO_COLOR takes the styling out of the live view and leaves the cursor movement it cannot redraw without; not parallel, it sets the environment and captures stdout.
func TestTheLiveViewHonoursNOCOLOR(t *testing.T) {
	sgr := regexp.MustCompile(`\x1b\[[0-9;]*m`)

	for noColor, styled := range map[string]bool{"": true, "1": false} {
		t.Setenv("NO_COLOR", noColor)

		var err error

		out := captureStdout(t, func() { err = Run([]string{"run", flagFixture(t), "--progress", "tty"}) })
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		if got := sgr.MatchString(out); got != styled {
			t.Errorf("NO_COLOR=%q styled = %v, want %v:\n%q", noColor, got, styled, out)
		}
	}
}

// The live view is as wide as the terminal says, and the fallback is for a terminal that says nothing or nothing useful.
func TestSizeOrTakesWhatTheTerminalReports(t *testing.T) {
	t.Parallel()

	cols := func(size *unix.Winsize) uint16 { return size.Col }

	for _, tc := range []struct {
		name string
		size unix.Winsize
		err  error
		want int
	}{
		{"a sized terminal", unix.Winsize{Col: 132}, nil, 132},
		{"a terminal reporting zero", unix.Winsize{}, nil, 80},
		{"not a terminal", unix.Winsize{}, errors.New("inappropriate ioctl for device"), 80},
	} {
		if got := sizeOr(80, cols, &tc.size, tc.err); got != tc.want {
			t.Errorf("%s: width %d, want %d", tc.name, got, tc.want)
		}
	}
}
