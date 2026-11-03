package agent

import "testing"

func TestResultKeysAreWireSpelled(t *testing.T) {
	t.Parallel()

	for constant, want := range map[string]string{
		resultErrorKey: "error", resultExitCodeKey: "exit_code", resultVerdictKey: "verdict", resultNoteKey: "note",
		resultResultKey: "result", resultContentKey: "content", resultTruncatedKey: "truncated", resultMessageKey: "message",
	} {
		if constant != want {
			t.Errorf("result key %q, want the wire spelling %q", constant, want)
		}
	}
}
