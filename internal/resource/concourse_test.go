package resource

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// A Concourse image takes its credentials in source:, so a resource's secrets: are laid over it in the request on stdin — the one place they go, never the script — and a first check carries no version, as Concourse sends none.
func TestConcourseRequestCarriesSecretsInSourceAndOmitsAnEmptyVersion(t *testing.T) {
	t.Parallel()

	ctx := shell.WithSecretEnv(context.Background(), map[string]string{"private_key": "s3cr3t"})
	call := Call{
		Type:     config.ResourceType{Name: "git", Config: config.ResourceTypeConfig{Check: "exec /opt/resource/check", Concourse: true}},
		Resource: config.Resource{Name: "repo", Source: map[string]any{"uri": "git@example.com:a/b"}},
	}

	request, err := concourseRequest(ctx, call, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]map[string]any

	err = json.Unmarshal([]byte(request), &decoded)
	if err != nil {
		t.Fatal(err)
	}

	if decoded["source"]["private_key"] != "s3cr3t" || decoded["source"]["uri"] != "git@example.com:a/b" {
		t.Errorf("source = %v, want the resource's own keys and its secret", decoded["source"])
	}

	for _, key := range []string{"version", "params"} {
		if _, sent := decoded[key]; sent {
			t.Errorf("a first check sent %s: %s", key, request)
		}
	}

	if strings.Contains(call.Type.Config.Check, "s3cr3t") || call.Resource.Source["private_key"] != nil {
		t.Error("the secret leaked into the script or the configured source")
	}
}

// An in is asked about one version, with the get's params.
func TestConcourseRequestCarriesAnInsVersionAndParams(t *testing.T) {
	t.Parallel()

	call := Call{Resource: config.Resource{Name: "repo", Source: map[string]any{"uri": "u"}}}

	request, err := concourseRequest(context.Background(), call, map[string]any{"ref": "abc"}, map[string]any{"depth": 1})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(request, `"version":{"ref":"abc"}`) || !strings.Contains(request, `"params":{"depth":1}`) {
		t.Errorf("an in's request = %s, want its version and params", request)
	}
}

// Concourse refuses an answer with no version; so does steps, and metadata becomes nothing but the version it rode with.
func TestUnwrapConcourseAnswer(t *testing.T) {
	t.Parallel()

	version, err := unwrapConcourseAnswer(context.Background(), []byte(`{"version":{"ref":"abc"},"metadata":[{"name":"author","value":"jt"}]}`))
	if err != nil || version["ref"] != "abc" || len(version) != 1 {
		t.Errorf("unwrapped %v, %v; want only the version", version, err)
	}

	for _, answer := range []string{`{"version":null}`, `{}`, `not json`} {
		_, err = unwrapConcourseAnswer(context.Background(), []byte(answer))
		if err == nil {
			t.Errorf("%s was accepted", answer)
		}
	}
}
