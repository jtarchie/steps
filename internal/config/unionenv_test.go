package config

import (
	"reflect"
	"testing"
)

// TestUnionEnvWithMoreExtraThanTypeNames: a type with no env: of its own is the common case for a resource adding one.
func TestUnionEnvWithMoreExtraThanTypeNames(t *testing.T) {
	t.Parallel()

	if got := UnionEnv(nil, []string{"A", "B", "A"}); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Errorf("UnionEnv = %v, want [A B]", got)
	}
}
