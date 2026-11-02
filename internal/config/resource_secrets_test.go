package config

import (
	"slices"
	"testing"
)

// A secret's name joins env: in what a resource keys on, so naming one more variable moves the key; the reference never does, since it decides access to a version and not what the version is.
func TestResourceHashedEnvIsNamesOnly(t *testing.T) {
	t.Parallel()

	r := Resource{Env: []string{"A"}, Secrets: map[string]string{"PUBLISH_AS": "((ci:x))"}}
	other := Resource{Env: []string{"A"}, Secrets: map[string]string{"PUBLISH_AS": "((ci:y))"}}

	if !slices.Equal(r.HashedEnv(), []string{"A", "PUBLISH_AS"}) {
		t.Errorf("HashedEnv = %v, want env and the secret's name", r.HashedEnv())
	}

	if !slices.Equal(r.HashedEnv(), other.HashedEnv()) {
		t.Error("a different reference under the same name changed what the resource keys on")
	}
}
