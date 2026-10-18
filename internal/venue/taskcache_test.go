package venue

import (
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestBareStepWithoutCachesSpendsNoRoundTrip: the zero session has no connection, so any exec here panics.
func TestBareStepWithoutCachesSpendsNoRoundTrip(t *testing.T) {
	t.Parallel()

	s := &bareSession{}

	err := s.attachCaches(WithLiveTaskCaches(t.Context(), "p", nil))
	if err != nil {
		t.Fatal(err)
	}
}

// TestBareCloseFilesBackOnlyAttachedCaches: until attachCaches has run, a cache path inside an input holds the input's upload, and filing that back would replace the worker's cache with it.
func TestBareCloseFilesBackOnlyAttachedCaches(t *testing.T) {
	t.Parallel()

	s := &bareSession{dir: "/w/steps-build.x", spec: shell.RunnerSpec{Caches: map[string]string{"src/node_modules": "abc"}}}

	if script := s.detachCaches(); script != "" {
		t.Errorf("close would file back a cache never attached: %q", script)
	}

	s.cachesAttached = true

	if s.detachCaches() == "" {
		t.Error("close would leave an attached cache behind")
	}
}
