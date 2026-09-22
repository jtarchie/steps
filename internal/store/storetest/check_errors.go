package storetest

// check_errors: why a resource cannot be checked, which is the one thing a
// failing poll used to leave nowhere but a log line.

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func checkErrorNames(t *testing.T, st store.Store) []string {
	t.Helper()

	errs, err := st.CheckErrors(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(errs))
	for _, row := range errs {
		names = append(names, row.Name)
	}

	return names
}

// TestACheckErrorIsRecordedAndCleared: the clear is the half that matters. A
// resource that recovers has to stop being reported, or the surface built on
// this is a light nobody can turn off.
func (s suite) TestACheckErrorIsRecordedAndCleared(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := t.Context()

	err := st.RecordCheckError(ctx, "repo", "check: exit status 128")
	if err != nil {
		t.Fatal(err)
	}

	errs, err := st.CheckErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(errs) != 1 || errs[0].Name != "repo" || errs[0].Message != "check: exit status 128" {
		t.Fatalf("CheckErrors = %+v, want one repo row carrying its message", errs)
	}

	if errs[0].FailedAt.IsZero() {
		t.Error("a recorded check error does not say when it failed")
	}

	err = st.RecordCheckError(ctx, "repo", "")
	if err != nil {
		t.Fatal(err)
	}

	if names := checkErrorNames(t, st); len(names) != 0 {
		t.Errorf("CheckErrors = %v after a clear, want none", names)
	}
}

// TestClearingACheckErrorThatIsNotThereIsFine: every successful poll of every
// healthy resource takes this path, so it has to be a no-op rather than a row
// count somebody checks.
func (s suite) TestClearingACheckErrorThatIsNotThereIsFine(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.RecordCheckError(t.Context(), "never-failed", "")
	if err != nil {
		t.Fatalf("clearing an absent check error: %v", err)
	}
}

// TestARecheckReplacesTheReasonRatherThanAddingOne: a resource down for an
// hour is one broken resource, not sixty.
func (s suite) TestARecheckReplacesTheReasonRatherThanAddingOne(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := t.Context()

	for _, message := range []string{"dial tcp: connection refused", "exit status 128"} {
		err := st.RecordCheckError(ctx, "repo", message)
		if err != nil {
			t.Fatal(err)
		}
	}

	errs, err := st.CheckErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(errs) != 1 || errs[0].Message != "exit status 128" {
		t.Fatalf("CheckErrors = %+v, want one row holding the newest reason", errs)
	}
}

// TestACheckErrorIsBoundedLikeEveryOtherStoredError: a check reports the whole
// generated shell script when it fails, and a resource down at a one-minute
// poll rewrites that row every minute.
func (s suite) TestACheckErrorIsBoundedLikeEveryOtherStoredError(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := t.Context()

	err := st.RecordCheckError(ctx, "repo", strings.Repeat("x", 4*store.MaxStoredErrorBytes))
	if err != nil {
		t.Fatal(err)
	}

	errs, err := st.CheckErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(errs) != 1 {
		t.Fatalf("CheckErrors = %+v, want one row", errs)
	}

	if len(errs[0].Message) > store.MaxStoredErrorBytes {
		t.Errorf("a stored check error is %d bytes, over the %d cap", len(errs[0].Message), store.MaxStoredErrorBytes)
	}
}

// TestCheckErrorsAreScopedToOnePipeline: two pipelines in one state file both
// name a resource `repo`, and the badge on one must not count the other's.
func (s suite) TestCheckErrorsAreScopedToOnePipeline(t *testing.T) {
	t.Parallel()

	web, infra := s.open(t, "web"), s.open(t, "infra")

	err := web.RecordCheckError(t.Context(), "repo", "web is broken")
	if err != nil {
		t.Fatal(err)
	}

	if names := checkErrorNames(t, infra); len(names) != 0 {
		t.Errorf("infra sees web's check errors: %v", names)
	}

	if names := checkErrorNames(t, web); len(names) != 1 {
		t.Errorf("web lost its own check error: %v", names)
	}
}
