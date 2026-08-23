package shim

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestSuppliedValuesDoNotDisplaceTheWorkersOwn pins which end owns the
// baseline.
//
// A step's env: names variables, and a venue resolves their values on the
// ORCHESTRATOR — that is the only machine where the operator's environment
// exists. But the baseline (PATH, HOME, TMPDIR, USER, SHELL, LANG) has to come
// from the machine the command runs on: a macOS orchestrator's HOME and TMPDIR
// name directories a Linux worker does not have, and mktemp, git and ssh break
// on them.
//
// Values were appended after the baseline, and os/exec keeps the LAST
// duplicate, so naming any allowlisted variable in env: silently replaced the
// worker's with the orchestrator's. Silent because it is a genuine no-op
// locally — the two values are the same machine's — and only misbehaves once a
// step is placed.
func TestSuppliedValuesDoNotDisplaceTheWorkersOwn(t *testing.T) {
	t.Setenv("PATH", "/worker/bin")
	t.Setenv("HOME", "/worker/home")

	env := shell.HostEnvWithValues(map[string]string{
		"PATH":             "/orchestrator/bin",
		"HOME":             "/orchestrator/home",
		"STEPS_TEST_OPTED": "carried",
	})

	if got := effective(env, "PATH"); got != "/worker/bin" {
		t.Errorf("PATH = %q, want the worker's own", got)
	}

	if got := effective(env, "HOME"); got != "/worker/home" {
		t.Errorf("HOME = %q, want the worker's own", got)
	}

	// A variable that is not part of the baseline is exactly what env: is for,
	// and must still arrive.
	if got := effective(env, "STEPS_TEST_OPTED"); got != "carried" {
		t.Errorf("STEPS_TEST_OPTED = %q, want the opted-in value", got)
	}
}

// effective is the value a process would see: os/exec keeps the last duplicate.
func effective(env []string, name string) string {
	value := ""

	for _, entry := range env {
		key, rest, found := strings.Cut(entry, "=")
		if found && key == name {
			value = rest
		}
	}

	return value
}
