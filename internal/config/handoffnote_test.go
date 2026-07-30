package config

import (
	"strings"
	"testing"
)

// handoffNotePipeline builds a two-agent pipeline whose plan body is supplied
// by the caller, so each test varies only the thing it is about.
func handoffNotePipeline(plan string) string {
	return `
agents:
- name: writer
  source: { model: openai/gpt-4o }
  tools: [read_file, write_file]
- name: reader
  source: { model: openai/gpt-4o }
  tools: [read_file]
- name: blind
  source: { model: openai/gpt-4o }
  tools: [write_file]
tasks:
- name: gate
  run: "true"
jobs:
- name: build
  plan:
` + plan
}

func loadHandoffNote(t *testing.T, plan string) (*Config, error) {
	t.Helper()

	return LoadConfig(writeConfig(t, handoffNotePipeline(plan)))
}

// TestHandoffNoteResolvesReceiver checks the happy path: the receiving step's
// HandoffNoteFrom is computed at load, and the sender itself receives nothing.
func TestHandoffNoteResolvesReceiver(t *testing.T) {
	t.Parallel()

	cfg, err := loadHandoffNote(t, `
  - agent: writer
    handoff_note: true
  - agent: reader
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	plan := cfg.Jobs[0].Plan
	if got := plan[0].HandoffNoteFrom; got != "" {
		t.Errorf("sender HandoffNoteFrom = %q, want empty", got)
	}

	if got := plan[1].HandoffNoteFrom; got != "writer" {
		t.Errorf("receiver HandoffNoteFrom = %q, want %q", got, "writer")
	}
}

// TestHandoffNoteCarriesAcrossTaskStep is the build-check case: a non-agent
// step between sender and receiver must not break the chain.
func TestHandoffNoteCarriesAcrossTaskStep(t *testing.T) {
	t.Parallel()

	cfg, err := loadHandoffNote(t, `
  - agent: writer
    handoff_note: true
  - task: gate
  - agent: reader
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := cfg.Jobs[0].Plan[2].HandoffNoteFrom; got != "writer" {
		t.Errorf("HandoffNoteFrom = %q, want %q across an intervening task", got, "writer")
	}
}

// TestHandoffNoteChainsThroughMiddleAgent checks a step that both receives and
// sends: it takes the previous sender, then becomes the sender for the next.
func TestHandoffNoteChainsThroughMiddleAgent(t *testing.T) {
	t.Parallel()

	cfg, err := loadHandoffNote(t, `
  - agent: writer
    handoff_note: true
  - agent: reader
    handoff_note: true
  - agent: reader
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	plan := cfg.Jobs[0].Plan
	if got := plan[1].HandoffNoteFrom; got != "writer" {
		t.Errorf("middle step HandoffNoteFrom = %q, want %q", got, "writer")
	}

	if got := plan[2].HandoffNoteFrom; got != "reader" {
		t.Errorf("last step HandoffNoteFrom = %q, want %q", got, "reader")
	}
}

// TestHandoffNoteRejections covers every load-time rejection in one table:
// each is dead or undeliverable config that must fail loudly rather than
// silently doing nothing at run time.
func TestHandoffNoteRejections(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ plan, wantErr string }{
		"no receiver in segment": {`
  - agent: reader
  - agent: writer
    handoff_note: true
`, "no later agent step in this segment receives it"},

		"receiver only after a get boundary": {`
  - agent: writer
    handoff_note: true
  - get: thing
  - agent: reader
`, "no later agent step in this segment receives it"},

		"on a task step": {`
  - task: gate
    handoff_note: true
  - agent: reader
`, "only valid on agent steps"},

		"receiver cannot read_file": {`
  - agent: writer
    handoff_note: true
  - agent: blind
`, "does not grant read_file"},

		// dir: moves a step's working directory off the build root, which is
		// where the note lives — the sender would write it inside an input
		// artifact and the receiver could never reach it (resolveAgentPath
		// rejects ".."). Silent today, so it has to be a load error.
		"sender sets dir": {`
  - agent: writer
    dir: sub
    handoff_note: true
  - agent: reader
`, "cannot set dir:"},

		"receiver sets dir": {`
  - agent: writer
    handoff_note: true
  - agent: reader
    dir: sub
`, "cannot set dir:"},

		// A note is addressed by step name, so two senders sharing one would
		// write the same file and fool the "nothing receives it" check.
		"two senders share a name": {`
  - agent: writer
    handoff_note: true
  - agent: writer
    handoff_note: true
  - agent: reader
`, "two steps named"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := loadHandoffNote(t, test.plan)
			if err == nil {
				t.Fatalf("LoadConfig succeeded, want error containing %q", test.wantErr)
			}

			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// TestHandoffNoteRejectedOnHook checks the hook case separately: a hook is a
// reaction, not a positioned step with a successor.
func TestHandoffNoteRejectedOnHook(t *testing.T) {
	t.Parallel()

	_, err := loadHandoffNote(t, `
  - agent: writer
    handoff_note: true
    on_failure:
      agent: reader
      handoff_note: true
  - agent: reader
`)
	if err == nil || !strings.Contains(err.Error(), "not valid on hook steps") {
		t.Fatalf("error = %v, want it to reject handoff_note on a hook", err)
	}
}

// TestHandoffNoteRejectedUnderWorkspaceIsolation guards the reason this is
// rejected rather than silently broken: under an isolated strategy only
// DECLARED outputs survive a step, so a note written to the build root would
// be discarded with the sender's own workspace.
func TestHandoffNoteRejectedUnderWorkspaceIsolation(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
workspace:
  strategy: copy
`+strings.TrimPrefix(handoffNotePipeline(`
  - agent: writer
    handoff_note: true
  - agent: reader
`), "\n"))

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "not supported with workspace strategy") {
		t.Fatalf("error = %v, want it to reject handoff_note under workspace isolation", err)
	}
}

// TestHandoffNoteDirIsReservedArtifactName guards the note directory against
// an artifact of the same name materializing over it in the shared build root.
func TestHandoffNoteDirIsReservedArtifactName(t *testing.T) {
	t.Parallel()

	err := ValidateArtifactName(HandoffNoteDir)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("ValidateArtifactName(%q) = %v, want a reserved-name error", HandoffNoteDir, err)
	}
}

// TestHandoffNoteRejectsReservedResourceName covers the hole ValidateArtifactName
// alone leaves: under the shared strategy a get materializes into
// <build>/<resource name> without ever passing through that check, so a resource
// named "handoff" would have the note written straight into the fetched
// resource. Only a config that actually uses handoff_note: is rejected — the
// name stays legal for everyone else.
func TestHandoffNoteRejectsReservedResourceName(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: "true"

resources:
- name: handoff
  type: dummy
  source: {}
`+strings.TrimPrefix(handoffNotePipeline(`
  - agent: writer
    handoff_note: true
  - agent: reader
`), "\n"))

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "reserved for handoff notes") {
		t.Fatalf("error = %v, want it to reject a resource named %q alongside handoff_note", err, HandoffNoteDir)
	}
}
