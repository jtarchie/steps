package hostenv

import (
	"slices"
	"strings"
	"testing"
)

// TestWithOptsInNamedVariables covers the env: escape hatch on the host
// path: allowlist stays the default trust boundary, and a pipeline can
// name one more variable through it without widening that default for
// everything else.
func TestWithOptsInNamedVariables(t *testing.T) {
	t.Setenv("STEPS_TEST_OPTED_IN", "yes")
	t.Setenv("STEPS_TEST_NOT_OPTED_IN", "no")

	env := With([]string{"STEPS_TEST_OPTED_IN"})

	if !slices.Contains(env, "STEPS_TEST_OPTED_IN=yes") {
		t.Errorf("env = %v, want the opted-in variable to be present", env)
	}

	for _, kv := range env {
		if strings.HasPrefix(kv, "STEPS_TEST_NOT_OPTED_IN=") {
			t.Errorf("env = %v, want a variable that was not named to stay out", env)
		}
	}
}

// TestWithSkipsUnsetNames pins that naming a variable nobody exported
// contributes nothing rather than an empty value: a command testing for
// presence must be able to tell "unset" from "set to empty", and inventing the
// latter would turn a forgotten export into a silent misconfiguration.
func TestWithSkipsUnsetNames(t *testing.T) {
	env := With([]string{"STEPS_TEST_DEFINITELY_UNSET"})

	for _, kv := range env {
		if strings.HasPrefix(kv, "STEPS_TEST_DEFINITELY_UNSET") {
			t.Errorf("env = %v, want an unset name to contribute nothing", env)
		}
	}
}

// TestEnvUnchangedWithoutOptIn keeps the no-env: case byte-identical to
// what Env always returned.
func TestEnvUnchangedWithoutOptIn(t *testing.T) {
	if got, want := strings.Join(With(nil), "\n"), strings.Join(Env(), "\n"); got != want {
		t.Errorf("With(nil) = %q, want it identical to Env() = %q", got, want)
	}
}

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

	env := WithValues(map[string]string{
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
