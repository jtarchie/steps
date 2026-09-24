package pipeline

import (
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// config cannot import shell, so the names exist twice; this keeps the lists equal.
func TestReservedEnvNamesMatchBuildMetadata(t *testing.T) {
	t.Parallel()

	got, want := config.ReservedEnvNames(), shell.BuildEnvNames()

	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("config refuses %v, shell sets %v", got, want)
	}
}
