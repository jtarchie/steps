package pipeline

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestAFanInNothingPassedTogetherResolvesToNothing: each version passed upstream, never in one build, so a latest get has nothing to build — failing the get rather than building an untested pair — while a version: every get and a rerun's history check still see every green version.
func TestAFanInNothingPassedTogetherResolvesToNothing(t *testing.T) {
	t.Parallel()

	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	err = st.RecordPassedVersions(t.Context(), []store.Pass{
		{JobName: "upstream", Resource: "repo", Version: `{"r":"r1"}`, BuildID: "build-1"},
		{JobName: "upstream", Resource: "config", Version: `{"c":"c1"}`, BuildID: "build-2"},
		{JobName: "upstream", Resource: "items", Version: `{"n":"1"}`, BuildID: "build-2"},
	})
	if err != nil {
		t.Fatal(err)
	}

	job := &config.Job{Name: "deploy", Plan: []config.Step{
		{Get: "repo", Passed: []string{"upstream"}},
		{Get: "config", Passed: []string{"upstream"}},
		{Get: "items", Passed: []string{"upstream"}, Version: "every"},
	}}

	history, err := loadResourceHistory(t.Context(), st, job)
	if err != nil {
		t.Fatal(err)
	}

	for _, check := range []struct{ what, got, want string }{
		{"resolvable(repo)", fmt.Sprint(history.resolvable("repo")), "[]"},
		{"resolvable(items)", fmt.Sprint(history.resolvable("items")), "[map[n:1]]"},
		{"get(repo)", fmt.Sprint(history.get("repo")), "[map[r:r1]]"},
	} {
		if check.got != check.want {
			t.Errorf("%s = %s, want %s", check.what, check.got, check.want)
		}
	}
}
