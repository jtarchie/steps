package pipeline

import (
	"errors"
	"reflect"
	"testing"
)

// TestEnsembleRecordKeepsWhatTheDecisionWasMadeFrom: the stored result names each member's vote, its note only when it gave one, and the verdict only when one came out.
func TestEnsembleRecordKeepsWhatTheDecisionWasMadeFrom(t *testing.T) {
	t.Parallel()

	votes := []memberVote{
		{agent: "a", verdict: "approve", note: "looks fine"},
		{agent: "b", verdict: "reject"},
		{agent: "c", err: errors.New("provider down")},
	}

	members := []any{
		map[string]any{"agent": "a", "verdict": "approve", "note": "looks fine"},
		map[string]any{"agent": "b", "verdict": "reject"},
		map[string]any{"agent": "c", "error": "provider down"},
	}

	for name, c := range map[string]struct {
		verdict string
		want    map[string]any
	}{
		"decided":   {"approve", map[string]any{"members": members, "verdict": "approve"}},
		"undecided": {"", map[string]any{"members": members}},
	} {
		if got := ensembleRecord(votes, c.verdict); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: recorded %#v, want %#v", name, got, c.want)
		}
	}
}
