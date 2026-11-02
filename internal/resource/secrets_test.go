package resource

import (
	"context"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// Two pipelines can hold the identical check and resolve the same name to different credentials; a shared answer would hand one pipeline the other's versions.
func TestACheckWithSecretsIsNeverShared(t *testing.T) {
	t.Parallel()

	ctx := WithCheckShare(context.Background(), NewCheckShare(time.Hour))

	call := Call{
		Type:     config.ResourceType{Name: "guarded", Config: config.ResourceTypeConfig{Check: `printf '[{"t":"%s"}]' "$PUBLISH_AS"`}},
		Resource: config.Resource{Name: "feed", Secrets: map[string]string{"PUBLISH_AS": "((ci:t))"}},
	}

	versionAs := func(value string) any {
		lookup := WithSecretLookup(ctx, func(context.Context, string, map[string]string) (map[string]string, error) {
			return map[string]string{"PUBLISH_AS": value}, nil
		})

		versions, err := CheckVersions(lookup, call)
		if err != nil || len(versions) != 1 {
			t.Fatalf("CheckVersions = %v, %v", versions, err)
		}

		return versions[0]["t"]
	}

	first, second := versionAs("pipeline-a"), versionAs("pipeline-b")
	if first != "pipeline-a" || second != "pipeline-b" {
		t.Errorf("checks answered %v then %v, want each pipeline's own", first, second)
	}
}
