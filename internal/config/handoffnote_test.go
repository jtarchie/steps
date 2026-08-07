package config

import (
	"slices"
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
- name: reader2
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
    handoff: { note: true }
  - agent: reader
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	plan := cfg.Jobs[0].Plan
	if got := plan[0].HandoffNoteFrom; len(got) != 0 {
		t.Errorf("sender HandoffNoteFrom = %q, want empty", got)
	}

	if got := plan[1].HandoffNoteFrom; !slices.Equal(got, []string{"writer"}) {
		t.Errorf("receiver HandoffNoteFrom = %q, want %q", got, "writer")
	}
}

// TestHandoffNoteCarriesAcrossTaskStep is the build-check case: a non-agent
// step between sender and receiver must not break the chain.
func TestHandoffNoteCarriesAcrossTaskStep(t *testing.T) {
	t.Parallel()

	cfg, err := loadHandoffNote(t, `
  - agent: writer
    handoff: { note: true }
  - task: gate
  - agent: reader
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := cfg.Jobs[0].Plan[2].HandoffNoteFrom; !slices.Equal(got, []string{"writer"}) {
		t.Errorf("HandoffNoteFrom = %q, want %q across an intervening task", got, "writer")
	}
}

// TestHandoffNoteChainsThroughMiddleAgent checks a step that both receives and
// sends: it takes the previous sender, then becomes the sender for the next.
func TestHandoffNoteChainsThroughMiddleAgent(t *testing.T) {
	t.Parallel()

	cfg, err := loadHandoffNote(t, `
  - agent: writer
    handoff: { note: true }
  - agent: reader
    handoff: { note: true }
  - agent: reader
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	plan := cfg.Jobs[0].Plan
	if got := plan[1].HandoffNoteFrom; !slices.Equal(got, []string{"writer"}) {
		t.Errorf("middle step HandoffNoteFrom = %q, want %q", got, "writer")
	}

	if got := plan[2].HandoffNoteFrom; !slices.Equal(got, []string{"reader"}) {
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
    handoff: { note: true }
`, "no later agent step in this segment receives it"},

		"receiver only after a get boundary": {`
  - agent: writer
    handoff: { note: true }
  - get: thing
  - agent: reader
`, "no later agent step in this segment receives it"},

		"on a task step": {`
  - task: gate
    handoff: { note: true }
  - agent: reader
`, "only valid on agent steps"},

		"receiver cannot read_file": {`
  - agent: writer
    handoff: { note: true }
  - agent: blind
`, "does not grant read_file"},

		// dir: moves a step's working directory off the build root, which is
		// where the note lives — the sender would write it inside an input
		// artifact and the receiver could never reach it (resolveAgentPath
		// rejects ".."). Silent today, so it has to be a load error.
		"sender sets dir": {`
  - agent: writer
    dir: sub
    handoff: { note: true }
  - agent: reader
`, "cannot set dir:"},

		"receiver sets dir": {`
  - agent: writer
    handoff: { note: true }
  - agent: reader
    dir: sub
`, "cannot set dir:"},

		// A note is addressed by step name, so two senders sharing one would
		// write the same file and fool the "nothing receives it" check.
		"two senders share a name": {`
  - agent: writer
    handoff: { note: true }
  - agent: writer
    handoff: { note: true }
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
    handoff: { note: true }
    on_failure:
      agent: reader
      handoff: { note: true }
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
    handoff: { note: true }
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
    handoff: { note: true }
  - agent: reader
`), "\n"))

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "reserved for handoff notes") {
		t.Fatalf("error = %v, want it to reject a resource named %q alongside handoff_note", err, HandoffNoteDir)
	}
}

// TestHandoffNoteFansOutAndIn is the concurrent-block wiring (#38): the note
// pending before a block reaches EVERY branch (broadcast, safe because a note
// is read only), and the step after the block receives every note the branches
// sent (aggregate), in declaration order.
func TestHandoffNoteFansOutAndIn(t *testing.T) {
	t.Parallel()

	cfg, err := loadHandoffNote(t, `
  - agent: writer
    handoff: { note: true }
  - in_parallel:
      steps:
      - agent: reader
        handoff: { note: true }
      - agent: reader2
        handoff: { note: true }
  - agent: reader
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	branches := cfg.Jobs[0].Plan[1].InParallel.Steps

	// Broadcast: both branches were handed the pre-block sender's note.
	for i := range branches {
		if got := branches[i].HandoffNoteFrom; !slices.Equal(got, []string{"writer"}) {
			t.Errorf("branch %d HandoffNoteFrom = %v, want [writer]", i, got)
		}
	}

	// Aggregate: the step after the block receives both branches, in the order
	// the pipeline lists them — not the order they happen to finish.
	if got := cfg.Jobs[0].Plan[2].HandoffNoteFrom; !slices.Equal(got, []string{"reader", "reader2"}) {
		t.Errorf("fan-in HandoffNoteFrom = %v, want [reader reader2]", got)
	}
}

// TestHandoffNoteBlockWithNoSendersIsTransparent proves a block that sends
// nothing does not break a chain across it: the step after still receives what
// was pending before, the same way an intervening task does.
func TestHandoffNoteBlockWithNoSendersIsTransparent(t *testing.T) {
	t.Parallel()

	cfg, err := loadHandoffNote(t, `
  - agent: writer
    handoff: { note: true }
  - in_parallel:
      steps:
      - task: gate
        inputs: []
      - task: gate
        inputs: []
  - agent: reader
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := cfg.Jobs[0].Plan[2].HandoffNoteFrom; !slices.Equal(got, []string{"writer"}) {
		t.Errorf("HandoffNoteFrom = %v, want [writer] across a block that sends nothing", got)
	}
}

// TestHandoffNoteBranchSenderNeedsAReceiver proves the dead-config rule
// reaches into blocks: a branch that sends with nothing after the block to
// receive it is rejected, exactly as a top-level sender would be.
func TestHandoffNoteBranchSenderNeedsAReceiver(t *testing.T) {
	t.Parallel()

	_, err := loadHandoffNote(t, `
  - in_parallel:
      steps:
      - agent: reader
        handoff: { note: true }
      - task: gate
        inputs: []
  - task: gate
    inputs: []
`)
	if err == nil {
		t.Fatal("LoadConfig succeeded; a branch note nothing receives must be rejected")
	}

	if !strings.Contains(err.Error(), "no later agent step") {
		t.Errorf("error = %v, want it to name the missing receiver", err)
	}
}

// TestHandoffNoteBranchesMustHaveUniqueNames proves the name-is-the-address
// rule holds inside a block: two branches running the same agent would write
// the same handoff/<name>.md, so one note would silently overwrite the other.
func TestHandoffNoteBranchesMustHaveUniqueNames(t *testing.T) {
	t.Parallel()

	_, err := loadHandoffNote(t, `
  - in_parallel:
      steps:
      - agent: reader
        handoff: { note: true }
      - agent: reader
        handoff: { note: true }
  - agent: reader
`)
	if err == nil {
		t.Fatal("LoadConfig succeeded; two branches sending under one name must be rejected")
	}

	if !strings.Contains(err.Error(), "names must be unique") {
		t.Errorf("error = %v, want it to name the collision", err)
	}
}
