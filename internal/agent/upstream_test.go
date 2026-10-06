package agent

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestRenderUpstreamShowsOnlyWhatTheLevelAsksFor: from: is a reader's declared need, so a verdict-level reader is never shown the sender's note or response, and nothing is announced that is not there.
func TestRenderUpstreamShowsOnlyWhatTheLevelAsksFor(t *testing.T) {
	t.Parallel()

	full := Upstream{Verdict: "revise", Note: "tighten the intro", Response: "the whole essay"}
	bare := Upstream{Verdict: "revise"}

	cases := []struct {
		name    string
		level   config.FromLevel
		up      Upstream
		want    []string
		notWant []string
	}{
		{"verdict", config.FromVerdict, full, []string{"verdict: revise"}, []string{"note:", "the whole essay", "full response"}},
		{"note", config.FromNote, full, []string{"note: tighten the intro"}, []string{"the whole essay", "full response"}},
		{"full", config.FromFull, full, []string{"note: tighten the intro", "Its full response follows.\n\nthe whole essay"}, nil},
		{"note with none given", config.FromNote, bare, []string{"verdict: revise"}, []string{"note:"}},
		{"full with nothing to show", config.FromFull, bare, []string{"verdict: revise"}, []string{"note:", "full response"}},
	}

	for _, tc := range cases {
		got := RenderUpstream("critic", tc.level, tc.up)

		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: rendered %q, want %q in it", tc.name, got, want)
			}
		}

		for _, unwanted := range tc.notWant {
			if strings.Contains(got, unwanted) {
				t.Errorf("%s: rendered %q, want no %q", tc.name, got, unwanted)
			}
		}
	}
}

// TestRecordOutcomeNeedsARegisterAndAName: a run that installed no register delivers nothing rather than failing, and a step with no name is nobody a reader could ask for.
func TestRecordOutcomeNeedsARegisterAndAName(t *testing.T) {
	t.Parallel()

	RecordOutcome(t.Context(), "critic", Upstream{Verdict: "approve"})

	ctx := WithOutcomes(t.Context())
	RecordOutcome(ctx, "", Upstream{Verdict: "approve"})

	if up, found := LookupOutcome(ctx, ""); found {
		t.Errorf("an unnamed step was recorded: %+v", up)
	}
}
