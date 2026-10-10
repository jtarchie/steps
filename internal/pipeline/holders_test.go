package pipeline

import (
	"errors"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestDeferrableRefusesACollectingCell: a cell captures under its coordinates
// (findings/alpha), and the collected artifact is composed HERE — a tree left
// on the worker under that path is one no consumer of findings would find.
func TestDeferrableRefusesACollectingCell(t *testing.T) {
	t.Parallel()

	rt := config.ResolvedTask{Name: "cell", Outputs: []string{"findings"}}

	if !deferrable(config.Step{}, rt) {
		t.Fatal("an ordinary task's outputs should be deferrable")
	}

	if deferrable(config.Step{OutputSubdir: "alpha"}, rt) {
		t.Error("a collecting cell's outputs were deferred; the collection is a local reader")
	}
}

// TestDeferrableRefusesAStepThatReadsItsOwnOutputsHere: every other way a
// step reads what it produced on this machine. Each is one reason, and a
// deferred output would make each of them see an empty directory.
func TestDeferrableRefusesAStepThatReadsItsOwnOutputsHere(t *testing.T) {
	t.Parallel()

	base := config.ResolvedTask{Name: "unit", Outputs: []string{"out"}}

	for _, tc := range []struct {
		name string
		rt   config.ResolvedTask
		want bool
	}{
		{name: "an ordinary task", rt: base, want: true},
		{name: "an assert with no files", rt: withAssert(base, &config.Assert{Code: new(int)}), want: true},
		{name: "an assert on files", rt: withAssert(base, &config.Assert{Files: []string{"out/f"}}), want: false},
		{name: "a fix agent", rt: withFix(base), want: false},
	} {
		if got := deferrable(config.Step{}, tc.rt); got != tc.want {
			t.Errorf("%s: deferrable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func withAssert(rt config.ResolvedTask, assert *config.Assert) config.ResolvedTask {
	rt.Assert = assert

	return rt
}

func withFix(rt config.ResolvedTask) config.ResolvedTask {
	rt.Fix = &config.FixSpec{}

	return rt
}

type recordingHolder struct {
	workspace.BuildWorkspace
	held    map[string]workspace.RemoteArtifact
	refuses string
}

func (h *recordingHolder) HoldRemote(name string, remote workspace.RemoteArtifact) error {
	if name == h.refuses {
		return errors.New("refused " + name)
	}

	h.held[name] = remote

	return nil
}

// Every output the worker kept is recorded under the name it is captured as, not just the first — and a hold the workspace refuses fails the step rather than leaving a consumer an empty directory.
func TestHoldRemoteOutputsRecordsEveryKeptOutput(t *testing.T) {
	t.Parallel()

	outputs := []string{"report", "bin", "local"}
	mapping := map[string]string{"bin": "binaries"}
	kept := map[string]string{"report": "sha-r", "bin": "sha-b"}

	ws := &recordingHolder{held: map[string]workspace.RemoteArtifact{}}

	err := holdRemoteOutputs(ws, outputs, mapping, kept, "ssh://w1")
	if err != nil {
		t.Fatalf("holdRemoteOutputs: %v", err)
	}

	if len(ws.held) != 2 || ws.held["report"].Digest != "sha-r" || ws.held["binaries"].Digest != "sha-b" || ws.held["binaries"].Holder != "ssh://w1" {
		t.Errorf("held %+v, want report and binaries (bin mapped) held on ssh://w1, and the output the worker did not keep left alone", ws.held)
	}

	refusing := &recordingHolder{held: map[string]workspace.RemoteArtifact{}, refuses: "binaries"}

	err = holdRemoteOutputs(refusing, outputs, mapping, kept, "ssh://w1")
	if err == nil || !strings.Contains(err.Error(), "refused binaries") {
		t.Errorf("a refused hold returned %v, want the workspace's error", err)
	}
}
