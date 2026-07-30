package agent

// handoffnote.go implements the FORWARD handoff: a step declaring
// handoff_note: must call a synthesized write_handoff tool before its
// conversation may end, and the runner renders what it wrote — plus a
// computed, un-authorable record of the files it actually touched — to
// handoff/<step>.md, which the next agent step receives as a context block.
//
// This is the shift-change report. It exists because the alternative — export
// the sender's recorded response and tool-call trajectory to the receiver —
// moves content across the tool-grant boundary the pipeline author drew (a
// response can quote shell/MCP output the receiver has no grant for) and
// anchors a reviewer on the implementer's own self-assessment. Here the
// sender deliberately authors what crosses, on a form this package owns, and
// nothing else from its run flows at all.
//
// The form is fixed rather than configurable: field names and descriptions
// are the opinion, so a pipeline is one boolean, not a schema.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

// writeHandoffToolName is the fixed name of the tool a handoff_note: step
// must call. A custom tool of the same name is rejected at injection, the
// same treatment verdictToolName and previousRunToolName get.
const writeHandoffToolName = "write_handoff"

// handoffNoteFields is the form, in render order. Descriptions are what the
// model reads while filling each one in, so they carry the discipline the
// note exists to enforce: inventory over self-assessment, file:line over
// prose, dead ends stated so the reader doesn't repeat them.
var handoffNoteFields = []struct{ name, description string }{ //nolint:gochecknoglobals // static, read-only form definition
	{
		"done",
		"What you did and where things stand. A factual inventory with file:line — not a grade. " +
			"Do not claim success; the next agent verifies that itself.",
	},
	{
		"facts",
		"What you learned that the next agent needs, with file:line. Include DEAD ENDS — what you " +
			"read and ruled out, one line each — so the next agent does not repeat the search.",
	},
	{
		"watch_out",
		"Risks, uncertainties, and anything that will bite the next agent if they do not know it. " +
			"If you deviated from your instructions, say so here and say why.",
	},
}

// buildWriteHandoffTool synthesizes the required write_handoff tool. Like the
// verdict tool it runs no command — it is a pure capture, so a call always
// "succeeds" (exit_code 0, which requiredCallSucceeded recognizes) and needs
// no new enforcement code. The captured fields are echoed back under
// handoffNoteResultKey for runAgentConversation to pick up.
func buildWriteHandoffTool() (*genai.FunctionDeclaration, toolImpl) {
	properties := make(map[string]*genai.Schema, len(handoffNoteFields))
	required := make([]string, 0, len(handoffNoteFields))

	for _, field := range handoffNoteFields {
		properties[field.name] = &genai.Schema{Type: genai.TypeString, Description: field.description}
		required = append(required, field.name)
	}

	decl := &genai.FunctionDeclaration{
		Name: writeHandoffToolName,
		Description: "Write your handoff note for the next agent in this pipeline, who has none of your context " +
			"and cannot see this conversation. Call this once, last, when your work is done — you cannot finish without it.",
		Parameters: &genai.Schema{Type: genai.TypeObject, Properties: properties, Required: required},
	}

	impl := func(_ context.Context, args map[string]any, _ toolEnv) map[string]any {
		note := make(map[string]string, len(handoffNoteFields))
		for _, field := range handoffNoteFields {
			note[field.name] = stringArg(args, field.name)
		}

		return map[string]any{"exit_code": 0, handoffNoteResultKey: note}
	}

	return decl, impl
}

// handoffNoteResultKey is how a successful write_handoff call smuggles its
// captured fields back through the generic tool-result map to
// runAgentConversation — mirroring how the verdict tool returns "verdict".
const handoffNoteResultKey = "handoff_note"

// injectWriteHandoffTool appends the synthesized required write_handoff tool
// when step declares handoff_note:, returning the tool's name (or "" when it
// doesn't). A pre-existing tool of the same name is a conflict, rejected here
// rather than silently shadowed.
func injectWriteHandoffTool(step config.Step, decls *genai.Tool, registry map[string]toolImpl, required map[string]bool) (string, error) {
	if !step.HandoffNote {
		return "", nil
	}

	if _, exists := registry[writeHandoffToolName]; exists {
		return "", fmt.Errorf("declares handoff_note: but already defines a tool named %q", writeHandoffToolName)
	}

	decl, impl := buildWriteHandoffTool()
	decls.FunctionDeclarations = append(decls.FunctionDeclarations, decl)
	registry[writeHandoffToolName] = impl
	required[writeHandoffToolName] = true

	return writeHandoffToolName, nil
}

// markTrajectoryResults backfills the ok flag on one turn's freshly-recorded
// calls from that turn's results. turn and parts are index-aligned:
// toolResponseParts appends exactly one part per call, in order. A length
// mismatch would mean that contract changed, so it degrades to leaving every
// call marked unsuccessful rather than pairing a call with the wrong result.
func markTrajectoryResults(turn []recordedToolCall, parts []*genai.Part) {
	if len(turn) != len(parts) {
		return
	}

	for i, part := range parts {
		if part.FunctionResponse == nil {
			continue
		}

		turn[i].ok = requiredCallSucceeded(part.FunctionResponse.Response)
	}
}

// latestHandoffNote returns the note this turn wrote, or current when it
// wrote none — so the conversation loop keeps "last successful write wins"
// without a branch of its own, the way it does for the verdict.
func latestHandoffNote(parts []*genai.Part, current map[string]string) map[string]string {
	if written := handoffNoteFrom(parts); written != nil {
		return written
	}

	return current
}

// handoffNoteFrom returns the fields captured by a successful write_handoff
// call among this turn's results, or nil when there was none. A failed call
// (the tool returns {"error": ...} only on a shape it cannot happen to
// produce, but the check keeps the contract uniform) records nothing, so the
// required tool stays unsatisfied and the model is forced to try again.
//
// A provider may emit several parallel calls in one turn; the LAST successful
// one wins, matching both latestHandoffNote's across-turn rule and how
// trackToolResults resolves a turn with more than one verdict call.
func handoffNoteFrom(parts []*genai.Part) map[string]string {
	var latest map[string]string

	for _, part := range parts {
		if part.FunctionResponse == nil || part.FunctionResponse.Name != writeHandoffToolName {
			continue
		}

		if !requiredCallSucceeded(part.FunctionResponse.Response) {
			continue
		}

		if note, ok := part.FunctionResponse.Response[handoffNoteResultKey].(map[string]string); ok {
			latest = note
		}
	}

	return latest
}

// handoffNoteProvenance is the first line of every rendered note. The reader
// is another model, and what it is about to read is one model's claims, not
// the repository's ground truth — the same trust stance sanitizeHandoffNote
// takes toward a verdict note.
const handoffNoteProvenance = "> Model-authored by agent %q (job %q) — claims to verify, not facts. " +
	"The final section is computed by the runner and is the only part the author could not write.\n"

// computedFilesHeading titles the section built from the run's own record.
// It is stripped from authored text (see sanitizeNoteField) so only the
// renderer can produce it.
const computedFilesHeading = "## Files touched"

// maxHandoffFieldBytes caps one authored field and maxHandoffComputedBytes
// the computed section. The budget is arithmetic, not a guess: the whole
// rendered note is read back by the RECEIVING step through loadContextBlocks,
// which treats anything over maxReadFileBytes (100,000) as a HARD error — so
// a verbose sender would fail an innocent successor. Three fields plus the
// computed section plus the provenance header must therefore fit under that
// limit with room to spare: 3×20,000 + 20,000 + a few hundred < 100,000.
const (
	maxHandoffFieldBytes    = 20_000
	maxHandoffComputedBytes = 20_000
)

// renderHandoffNote formats one note: the provenance header, the authored
// fields in form order, then the computed files section. Authored text is
// sanitized (headings stripped) and capped, so neither a forged section nor a
// runaway field can reach the receiver.
func renderHandoffNote(agentName, jobName string, note map[string]string, trajectory []recordedToolCall) string {
	var b strings.Builder

	fmt.Fprintf(&b, handoffNoteProvenance, agentName, jobName)

	for _, field := range handoffNoteFields {
		value := strings.TrimSpace(note[field.name])
		if value == "" {
			value = "(nothing reported)"
		}

		fmt.Fprintf(&b, "\n## %s\n\n%s\n", field.name, sanitizeNoteField(value))
	}

	computed := truncateToolOutputLimit(renderTouchedFiles(trajectory), maxHandoffComputedBytes)

	fmt.Fprintf(&b, "\n%s (computed from the run, not authored)\n\n%s\n", computedFilesHeading, computed)

	return b.String()
}

// sanitizeNoteField prepares one authored field for embedding. Markdown ATX
// headings are demoted to bold text rather than dropped: the author may
// legitimately want structure, but a line starting with "##" could otherwise
// forge the computed section heading and pass model-authored text off as the
// runner's own record.
//
// An oversized field is TRUNCATED inline rather than spilled to a file: the
// spill directory belongs to the SENDING step and is removed the moment that
// step returns (see preparedAgentStep.close), so a spill pointer in a note
// would always name a file the receiver cannot open. A truncated field at
// least still carries what fits.
func sanitizeNoteField(value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			lines[i] = "**" + strings.TrimLeft(trimmed, "# ") + "**"
		}
	}

	return truncateToolOutputLimit(strings.Join(lines, "\n"), maxHandoffFieldBytes)
}

// pathArgKeys are the argument names the builtin file tools use for the path
// they act on. Only these are harvested into the computed section: an
// arbitrary model-authored string from some other tool's arguments (a
// run_shell command line, an MCP query) must never reach the receiver
// through a section labelled as the runner's own record.
var pathArgKeys = map[string]bool{"path": true, "file_path": true} //nolint:gochecknoglobals // static, read-only lookup table

// fileToolNames are the tools whose path argument names a workspace file the
// agent actually read or wrote.
var fileToolNames = map[string]bool{ //nolint:gochecknoglobals // static, read-only lookup table
	"read_file": true, "write_file": true, "edit_file": true, "list_dir": true, "search_files": true,
}

// renderTouchedFiles builds the computed section: deduped paths per file
// tool, then every other tool as a name and a count.
//
// Only calls that SUCCEEDED are counted — a write_file the model requested
// but that failed (or was rejected by a max_calls: budget without ever
// running) did not touch anything, and listing it would make this section
// exactly the kind of unverified claim it exists to be an antidote to.
//
// Non-file tools contribute a count and nothing else. That is what keeps a
// run_shell command line — or any other model-authored argument — structurally
// out of the receiver's context, rather than relying on the sender's
// discretion.
func renderTouchedFiles(trajectory []recordedToolCall) string {
	paths := map[string]map[string]bool{}
	others := map[string]int{}

	for _, call := range trajectory {
		if !call.ok {
			continue
		}

		if !fileToolNames[call.name] {
			others[call.name]++

			continue
		}

		path := pathArg(call.args)
		if path == "" {
			others[call.name]++

			continue
		}

		if paths[call.name] == nil {
			paths[call.name] = map[string]bool{}
		}

		paths[call.name][path] = true
	}

	var b strings.Builder

	for _, name := range sortedKeys(paths) {
		fmt.Fprintf(&b, "%s: %s\n", name, strings.Join(sortedSet(paths[name]), ", "))
	}

	if len(others) > 0 {
		parts := make([]string, 0, len(others))
		for _, name := range sortedKeys(others) {
			parts = append(parts, fmt.Sprintf("%s x%d", name, others[name]))
		}

		fmt.Fprintf(&b, "other tools: %s\n", strings.Join(parts, ", "))
	}

	if b.Len() == 0 {
		return "(no successful tool calls recorded)"
	}

	return strings.TrimRight(b.String(), "\n")
}

// pathArg returns the first path-shaped argument of a file tool call, or ""
// when the call carried none (a search_files without an explicit path, say).
func pathArg(args map[string]any) string {
	for key, value := range args {
		if !pathArgKeys[key] {
			continue
		}

		if path, ok := value.(string); ok && path != "" {
			return path
		}
	}

	return ""
}

// sortedKeys returns m's keys in sorted order, so a rendered note is
// deterministic across runs rather than following Go's map iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// sortedSet returns a set's members in sorted order.
func sortedSet(set map[string]bool) []string {
	return sortedKeys(set)
}

// publishHandoffNote renders and persists a finished sending step's note.
// A no-op for a step that declares no handoff_note:, and — defensively — for
// one whose model somehow finished without a successful write_handoff, which
// the required-tool machinery already prevents.
//
// A write failure is logged and swallowed: the note is a productivity aid for
// the next agent, and losing it must never discard an otherwise-successful
// agent run. The receiver treats an absent note the same way it treats a
// guard-skipped sender (see withHandoffNotePath), so the pipeline degrades to
// today's behavior rather than breaking.
func publishHandoffNote(prepared preparedAgentStep, jobName string, res conversationResult) {
	if !prepared.step.HandoffNote || res.handoffNote == nil {
		return
	}

	path, err := writeHandoffNote(
		prepared.conv.env.dir, prepared.step.Agent, jobName,
		res.handoffNote, res.trajectory,
	)
	if err != nil {
		slog.Warn("agent.handoff_note.write_failed", "agent", prepared.step.Agent, "error", err)

		return
	}

	slog.Info("agent.handoff_note", "agent", prepared.step.Agent, "path", path)
}

// writeHandoffNote renders and persists the note for a finished sending step,
// under HandoffNoteDir in the build workspace root. buildDir is the step's own
// working directory, which for the shared workspace strategy IS the build root
// — config.validateHandoffNoteSteps rejects handoff_note: under an isolated
// strategy, and rejects dir: on either end of a note edge, precisely because
// that equivalence is what makes the note reachable by the next step (whose
// own path resolution is relative to ITS working directory).
//
// A write failure is logged by the caller and never fails the step: the note
// is a productivity aid for the next agent, and losing it must not throw away
// a completed agent run.
func writeHandoffNote(buildDir, agentName, jobName string, note map[string]string, trajectory []recordedToolCall) (string, error) {
	dir := filepath.Join(buildDir, config.HandoffNoteDir)

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return "", fmt.Errorf("create handoff note dir: %w", err)
	}

	path := filepath.Join(dir, agentName+".md")

	err = os.WriteFile(path, []byte(renderHandoffNote(agentName, jobName, note, trajectory)), 0o600)
	if err != nil {
		return "", fmt.Errorf("write handoff note: %w", err)
	}

	return path, nil
}

// withHandoffNotePath appends the note addressed to step, if there is one, to
// its declared context_paths — so delivery reuses loadContextBlocks verbatim:
// same workspace confinement, same size cap, same zero-turn synthetic
// read_file injection. It goes FIRST, ahead of the author's own context
// paths, because it is the orientation the rest of the conversation builds on.
//
// A missing file is skipped rather than an error: the sending step may have
// been guard-skipped (when:) or merkle-skipped, and a receiver must not fail
// because its predecessor legitimately never ran. That is the one place this
// deliberately diverges from context_paths, where a missing file is a hard
// error because the author named it explicitly.
//
// Resolving from step.HandoffNoteFrom (computed at load) rather than from a
// carry through internal/pipeline is what makes delivery idempotent: every
// dispatch re-reads whatever is on disk, so a to:-driven redo picks up the
// newest note rather than a stale captured one.
func withHandoffNotePath(step config.Step, dir string, paths []string) []string {
	if step.HandoffNoteFrom == "" {
		return paths
	}

	rel := config.HandoffNotePath(step.HandoffNoteFrom)

	_, err := os.Stat(filepath.Join(dir, rel))
	if err != nil {
		slog.Debug("agent.handoff_note.absent", "from", step.HandoffNoteFrom, "path", rel)

		return paths
	}

	return append([]string{rel}, paths...)
}
