package resource

import (
	"context"
	"fmt"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/redact"
)

func assertFacts(t *testing.T, facts []events.Fact, want string) {
	t.Helper()

	if got := fmt.Sprint(facts); got != want {
		t.Errorf("facts = %s, want %s", got, want)
	}
}

// TestAnOutAnswerInConcoursesShapeIsAVersionAndFacts: the upgrade case — {"version": {...}} alone, or with metadata — is read as the version inside it; anything else stays the version it always was.
func TestAnOutAnswerInConcoursesShapeIsAVersionAndFacts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, out, version, facts string
	}{
		{"wrapped", `{"version": {"id": "7"}, "metadata": [{"name": "url", "value": "https://x/7"}, {"name": "n", "value": 3}]}`, `map[id:7]`, `[{url https://x/7} {n 3}]`},
		{"version alone", `{"version": {"id": "7"}}`, `map[id:7]`, `[]`},
		{"a version field that is a string", `{"version": "1.2"}`, `map[version:1.2]`, `[]`},
		{"another key beside version", `{"version": {"id": "7"}, "sha": "abc"}`, `map[sha:abc version:map[id:7]]`, `[]`},
		{"not an object", `[]`, `map[]`, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, facts := CollectFacts(context.Background())
			rt := config.ResourceType{Name: "dummy", Config: config.ResourceTypeConfig{Out: "printf '%s' '" + tc.out + "'"}}

			version, err := RunOut(ctx, Call{Type: rt, Resource: config.Resource{Source: map[string]any{}}, Params: map[string]any{}, Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}

			if got := fmt.Sprint(version); got != tc.version {
				t.Errorf("version = %s, want %s", got, tc.version)
			}

			if got := fmt.Sprint(facts()); got != tc.facts {
				t.Errorf("facts = %s, want %s", got, tc.facts)
			}
		})
	}
}

// TestAnInAnswerIsReadOnlyWhenItIsTheWholeOfStdout: an in: that prints its log on stdout is not answering, and a log line that happens to be JSON must not become a fact.
func TestAnInAnswerIsReadOnlyWhenItIsTheWholeOfStdout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, in, facts string
	}{
		{"the answer", `printf '{"version": {"id": "1"}, "metadata": [{"name": "url", "value": "https://x/1"}]}'`, `[{url https://x/1}]`},
		{"the answer, logging on stderr", `echo working >&2; printf '{"version": {"id": "1"}, "metadata": [{"name": "url", "value": "https://x/1"}]}'`, `[{url https://x/1}]`},
		{"a log before it", `echo fetching; printf '{"version": {"id": "1"}, "metadata": [{"name": "url", "value": "https://x/1"}]}'`, `[]`},
		{"a version with no metadata", `printf '{"version": {"id": "1"}}'`, `[]`},
		{"nothing", `true`, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, facts := CollectFacts(context.Background())
			rt := config.ResourceType{Name: "dummy", Config: config.ResourceTypeConfig{In: tc.in}}

			err := RunIn(ctx, Call{Type: rt, Resource: config.Resource{Source: map[string]any{}}, Version: map[string]any{"id": "1"}, Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}

			if got := fmt.Sprint(facts()); got != tc.facts {
				t.Errorf("facts = %s, want %s", got, tc.facts)
			}
		})
	}
}

// TestAConcourseAnswersMetadataIsItsFacts: the metadata a stock image answers with is reported, no longer said as a note of its own.
func TestAConcourseAnswersMetadataIsItsFacts(t *testing.T) {
	t.Parallel()

	ctx, facts := CollectFacts(context.Background())

	_, err := unwrapConcourseAnswer(ctx, []byte(`{"version":{"ref":"abc"},"metadata":[{"name":"author","value":"jt"}]}`))
	if err != nil {
		t.Fatal(err)
	}

	if got := fmt.Sprint(facts()); got != "[{author jt}]" {
		t.Errorf("facts = %s, want the answer's metadata", got)
	}
}

// TestReportedFactsAreMaskedUnderTheResourcesSecrets: a fact is kept beside its version, where no run's masking reaches it, so a token a resource echoed into a URL is masked where the fact is born.
func TestReportedFactsAreMaskedUnderTheResourcesSecrets(t *testing.T) {
	t.Parallel()

	secrets := &redact.Secrets{}
	secrets.Add("hunter2-token")

	ctx, facts := CollectFacts(redact.WithSecrets(context.Background(), secrets))
	reportFacts(ctx, []events.Fact{{Name: "url", Value: "https://x/?t=hunter2-token"}})

	if got := fmt.Sprint(facts()); got != "[{url https://x/?t=***}]" {
		t.Errorf("facts = %s, want the token masked", got)
	}
}
