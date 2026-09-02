package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveFileIncludes inlines every *_file field's contents (and every
// whole-document file: reference) into its sibling struct field, resolving
// each path relative to baseDir — the directory holding the pipeline YAML.
// It runs between yaml.Unmarshal and validate() (see LoadConfig), so every
// validator, every merkle content builder, and every executor afterwards sees
// a *Config indistinguishable from one whose text was written inline.
//
// baseDir is a parameter, never stored on Config: merkle hashing must stay a
// pure function of *Config, and the path a file was loaded from is
// deliberately not hashed — only the text it resolves to is, which is what
// actually determines a step's result (see TaskNodeContent/AgentContentMap).
// Renaming ci/a.sh to ci/b.sh with identical bytes correctly does not bust a
// cache.
//
// The one exception is an agent step's message_files: {artifact, path} mapping
// form (see FileRef.Deferred): that names a file inside an artifact a get
// step fetches, which does not exist yet at load time, so it is left
// untouched here and resolved later by internal/agent, once the artifact is
// on disk.
func (c *Config) resolveFileIncludes(baseDir string) ([]string, error) {
	resolver := &includeResolver{baseDir: baseDir}

	for i := range c.Tasks {
		err := resolveTaskIncludes(resolver, &c.Tasks[i])
		if err != nil {
			return nil, err
		}
	}

	for i := range c.Agents {
		err := resolveAgentIncludes(resolver, &c.Agents[i])
		if err != nil {
			return nil, err
		}
	}

	for i := range c.ResourceTypes {
		err := resolveResourceTypeIncludes(resolver, &c.ResourceTypes[i])
		if err != nil {
			return nil, err
		}
	}

	for i := range c.Jobs {
		err := c.Jobs[i].visitSteps(func(label string, step *Step) error {
			return resolveStepIncludes(resolver, label, step)
		})
		if err != nil {
			return nil, err
		}
	}

	return resolver.read, nil
}

// include describes one text field and the optional path that may supply its
// contents, for applyInclude.
type include struct {
	context string  // e.g. `task "unit"` or `job "build" step 2` — for error text
	key     string  // the YAML key this field's *_file sibling extends, e.g. "run"
	path    string  // the <key>_file value; empty means nothing to do
	target  *string // the sibling field to fill in place
}

// applyInclude reads inc.path (resolved relative to baseDir) into *inc.target
// when inc.path is set; a no-op when it's empty. Setting both the inline
// field and its *_file sibling is a load-time error, and so is an empty
// included file — either would silently change what the entry means (e.g. an
// empty run_file: would leave a task step's Run == "", making it fall through
// ResolveTask's inline short-circuit to a tasks: reference instead).
func applyInclude(resolver *includeResolver, inc include) error {
	if inc.path == "" {
		return nil
	}

	if *inc.target != "" {
		return fmt.Errorf("%s: %s: and %s_file: are mutually exclusive", inc.context, inc.key, inc.key)
	}

	data, err := resolver.readFile(inc.context, inc.key+"_file", inc.path)
	if err != nil {
		return err
	}

	*inc.target = string(data)

	return nil
}

// includeResolver reads a pipeline's *_file includes and remembers which
// files on disk it read.
//
// The list is what puts an include inside the pipeline's REVISION. The
// content of a run_file: or a system_file: decides what a step executes, so a
// hash taken over the YAML alone answers "did the pipeline change?" with a
// confident no for the edit that changed everything — see Load.
//
// @builtin/ includes are deliberately not listed: they are compiled into this
// binary, so no edit can move them while a daemon runs.
type includeResolver struct {
	baseDir string
	read    []string
}

// readFile resolves path relative to baseDir and reads it. A path may
// use ".." to escape baseDir: the pipeline file is trusted input (see
// LoadConfig's own os.ReadFile), and a file placed beside it by the same
// author is at the same trust level — a shared ../tasks/ directory next to a
// pipelines/ directory is a legitimate layout, not a hole to close.
//
// A path with the "@builtin/<name>" prefix reads from the embedded prompt
// library instead of from disk, so pipelines can reference the curated
// system prompts shipped with the binary without needing a sibling file.
//
// A not-found error carries a specific hint for the common Concourse-habit
// mistake (run_file: repo/ci/build.sh, meaning a fetched artifact): that path
// essentially never exists next to a pipeline YAML, so this fires in
// practice for exactly that case.
func (r *includeResolver) readFile(context, key, path string) ([]byte, error) {
	baseDir := r.baseDir

	if strings.HasPrefix(path, "@builtin/") {
		name := strings.TrimPrefix(path, "@builtin/")
		data, err := ReadBuiltinPrompt(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %s %q: %w", context, key, path, err)
		}
		return []byte(data), nil
	}

	if filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s: %s %q must be a path relative to the pipeline file's directory", context, key, path)
	}

	full := filepath.Join(baseDir, path)

	data, err := os.ReadFile(full) //nolint:gosec // resolved from the pipeline file's own directory, which is trusted input — same rationale as LoadConfig's read of the pipeline file itself
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %s %q: no such file relative to the pipeline directory %q "+
				"(a path naming a fetched artifact is not supported here — see docs/agents.md)", context, key, path, baseDir)
		}

		return nil, fmt.Errorf("%s: could not read %s %q: %w", context, key, path, err)
	}

	if strings.TrimSpace(string(data)) == "" {
		return nil, fmt.Errorf("%s: %s %q is empty", context, key, path)
	}

	r.read = append(r.read, full)

	return data, nil
}

// resolveResourceTypeIncludes applies a resource type's expr.check_file: /
// expr.in_file: / expr.out_file:.
//
// Only the expr backend has them. A shell check: is usually one or two lines
// and reads fine inline; an expression that walks three dependent API calls
// is twenty, and belongs in a file that a diff and a review comment can
// address.
func resolveResourceTypeIncludes(resolver *includeResolver, rt *ResourceType) error {
	if rt.Config.Expr == nil {
		return nil
	}

	context := fmt.Sprintf("resource_type %q expr", rt.Name)
	expression := rt.Config.Expr

	for _, inc := range []include{
		{context: context, key: "check", path: expression.CheckFile, target: &expression.Check},
		{context: context, key: "in", path: expression.InFile, target: &expression.In},
		{context: context, key: "out", path: expression.OutFile, target: &expression.Out},
	} {
		err := applyInclude(resolver, inc)
		if err != nil {
			return err
		}
	}

	return nil
}

// resolveTaskIncludes applies a tasks: entry's file: (a whole-document
// include, merged so the entry's own inline fields win) and its run_file:/
// fix.message_files:.
func resolveTaskIncludes(resolver *includeResolver, t *Task) error {
	context := fmt.Sprintf("task %q", t.Name)

	if t.File != "" {
		doc, err := loadTaskDocument(resolver, context, t.File)
		if err != nil {
			return err
		}

		mergeTaskDocument(t, doc)
	}

	err := applyInclude(resolver, include{context: context, key: "run", path: t.RunFile, target: &t.Run})
	if err != nil {
		return err
	}

	return applyFixInclude(resolver, context, t.Fix)
}

// loadTaskDocument reads and parses path as a standalone Task document. The
// document may not itself use file:/run_file: — includes are resolved one
// level deep only, which is what makes cycle detection unnecessary.
func loadTaskDocument(resolver *includeResolver, context, path string) (Task, error) {
	data, err := resolver.readFile(context, "file", path)
	if err != nil {
		return Task{}, err
	}

	var doc Task

	err = strictUnmarshal(data, &doc)
	if err != nil {
		return Task{}, fmt.Errorf("%s: file %q: %w", context, path, err)
	}

	if doc.File != "" || doc.RunFile != "" {
		return Task{}, fmt.Errorf("%s: file %q: an included task document may not itself set file or run_file", context, path)
	}

	return doc, nil
}

// mergeTaskDocument fills t's unset fields from doc, the document t.File
// loaded — the same "wins when set" idiom ResolveTask uses between a step and
// its tasks: entry. t.Name is never touched: the entry, not the document,
// names the task.
func mergeTaskDocument(t *Task, doc Task) {
	if t.Run == "" {
		t.Run = doc.Run
	}

	if t.Fix == nil {
		t.Fix = doc.Fix
	}

	if t.Image == "" {
		t.Image = doc.Image
	}

	if t.Timeout == "" {
		t.Timeout = doc.Timeout
	}

	if t.Inputs == nil {
		t.Inputs = doc.Inputs
	}

	if t.Outputs == nil {
		t.Outputs = doc.Outputs
	}
}

// resolveAgentIncludes applies an agents: entry's file: (a whole-document
// include, merged so the entry's own inline fields win) and its system_file:.
func resolveAgentIncludes(resolver *includeResolver, a *Agent) error {
	context := fmt.Sprintf("agent %q", a.Name)

	if a.File != "" {
		doc, err := loadAgentDocument(resolver, context, a.File)
		if err != nil {
			return err
		}

		mergeAgentDocument(a, doc)
	}

	return applyInclude(resolver, include{context: context, key: "system", path: a.SystemFile, target: &a.System})
}

// loadAgentDocument reads and parses path as a standalone Agent document. The
// document may not itself use file:/system_file: — includes are resolved one
// level deep only.
func loadAgentDocument(resolver *includeResolver, context, path string) (Agent, error) {
	data, err := resolver.readFile(context, "file", path)
	if err != nil {
		return Agent{}, err
	}

	var doc Agent

	err = strictUnmarshal(data, &doc)
	if err != nil {
		return Agent{}, fmt.Errorf("%s: file %q: %w", context, path, err)
	}

	if doc.File != "" || doc.SystemFile != "" {
		return Agent{}, fmt.Errorf("%s: file %q: an included agent document may not itself set file or system_file", context, path)
	}

	return doc, nil
}

// mergeAgentDocument fills a's unset fields from doc, the document a.File
// loaded. Source is treated as one unit (set as a whole from the document
// only when the entry declares no source: at all) rather than merged
// field-by-field, since a mix of an inline model: with a document's endpoint:
// is more likely a mistake than an intended override. a.Name is never
// touched: the entry, not the document, names the agent. Split into three
// parts purely to stay under the linter's cyclomatic-complexity budget —
// there is no grouping significance to the split.
func mergeAgentDocument(a *Agent, doc Agent) {
	mergeAgentIdentity(a, doc)
	mergeAgentDials(a, doc)
	mergeAgentLimits(a, doc)
}

func mergeAgentIdentity(a *Agent, doc Agent) {
	if a.Source == (AgentSource{}) {
		a.Source = doc.Source
	}

	if a.Description == "" {
		a.Description = doc.Description
	}

	if a.Image == "" {
		a.Image = doc.Image
	}

	if a.System == "" {
		a.System = doc.System
	}

	if len(a.Tools) == 0 {
		a.Tools = doc.Tools
	}
}

func mergeAgentDials(a *Agent, doc Agent) {
	if a.Temperature == nil {
		a.Temperature = doc.Temperature
	}

	if a.TopP == nil {
		a.TopP = doc.TopP
	}

	if a.MaxTokens == 0 {
		a.MaxTokens = doc.MaxTokens
	}

	if a.ReasoningEffort == "" {
		a.ReasoningEffort = doc.ReasoningEffort
	}

	if a.CompactAfterTokens == nil {
		a.CompactAfterTokens = doc.CompactAfterTokens
	}

	if a.ContextWindow == 0 {
		a.ContextWindow = doc.ContextWindow
	}
}

func mergeAgentLimits(a *Agent, doc Agent) {
	if a.MaxTurns == nil {
		a.MaxTurns = doc.MaxTurns
	}

	if a.MaxContextBytes == nil {
		a.MaxContextBytes = doc.MaxContextBytes
	}

	if a.Timeout == "" {
		a.Timeout = doc.Timeout
	}

	if a.Attempts == nil {
		a.Attempts = doc.Attempts
	}
}

// resolveStepIncludes applies one step's own includes: run_file: (task steps
// only), message_files: when it's the load-time scalar form (agent steps only —
// the {artifact, path} mapping form is deferred to run time; see FileRef and
// internal/agent), and its fix:'s message_files:.
func resolveStepIncludes(resolver *includeResolver, label string, step *Step) error {
	if step.RunFile != "" && step.Task == "" {
		return fmt.Errorf("%s: run_file: is only valid on task steps", label)
	}

	err := applyInclude(resolver, include{context: label, key: "run", path: step.RunFile, target: &step.Run})
	if err != nil {
		return err
	}

	err = resolveStepMessages(resolver, label, step)
	if err != nil {
		return err
	}

	return applyFixInclude(resolver, label, step.Fix)
}

// resolveStepMessages turns a step's message_files: into messages:, for the
// load-time form.
//
// The exclusivity check runs FIRST, and before the deferred/non-deferred split,
// which is the one behavioural change in this rename. It used to sit inside the
// non-deferred branch, so a step pairing messages: with a run-time {artifact,
// path} file was refused by internal/agent halfway through a build rather than
// by the loader — a pipeline that could never work, discovered after twenty
// minutes of fetching. Both forms now fail at load, where `steps validate` can
// see them.
func resolveStepMessages(resolver *includeResolver, label string, step *Step) error {
	if len(step.MessageFiles) > 0 && step.Agent == "" {
		return fmt.Errorf("%s: message_files: is only valid on agent steps", label)
	}

	if len(step.MessageFiles) == 0 {
		return nil
	}

	err := checkMessageFileForms(label, step.MessageFiles)
	if err != nil {
		return err
	}

	if len(step.Messages) > 0 {
		return fmt.Errorf("%s: messages: and message_files: are mutually exclusive — two ordered lists cannot say which message comes first", label)
	}

	if step.MessageFiles[0].Deferred() {
		// Read at run time out of an artifact this step declares, so there is
		// nothing to inline here; internal/agent resolves them in order.
		return nil
	}

	messages := make([]string, 0, len(step.MessageFiles))

	for _, ref := range step.MessageFiles {
		data, err := resolver.readFile(label, "message_files", ref.Path)
		if err != nil {
			return err
		}

		messages = append(messages, string(data))
	}

	step.Messages = messages
	step.MessageFiles = nil

	return nil
}

// checkMessageFileForms refuses the two shapes a list can take that a single
// *FileRef could not.
//
// A null entry decodes to a nil FileRef, and every site that reads one would
// dereference it — here, and again in internal/agent for the deferred form.
// The scalar field this replaced was nil-checked at each of them; a list
// rejects the hole once instead.
//
// A mix of forms is refused because the two resolve against different roots: a
// bare path against the pipeline file's directory at load time, an {artifact,
// path} against the step's materialized workspace at run time. One ordered
// list cannot be resolved at two times without carrying per-entry state
// through hashing and into internal/agent.
//
// Left unrefused, a mixed list was worse than broken: one deferred entry made
// the whole list skip inlining, so the bare path was later resolved under the
// workspace. It either vanished mid-build, or — where an input artifact
// happened to share the leading directory name — resolved to a file the
// FETCHED artifact supplied, sending a PR author's text to the model in place
// of the pipeline author's.
//
// Both are refused at LOAD, so a pipeline that cannot work is caught before
// any get has run.
func checkMessageFileForms(label string, refs []*FileRef) error {
	deferred, inline := 0, 0

	for i, ref := range refs {
		if ref == nil || (ref.Path == "" && ref.Artifact == "") {
			return fmt.Errorf("%s: message_files: entry %d is empty — remove it, or name a file", label, i+1)
		}

		if ref.Deferred() {
			deferred++
		} else {
			inline++
		}
	}

	if deferred > 0 && inline > 0 {
		return fmt.Errorf("%s: message_files: mixes file paths with {artifact, path} entries — a path resolves against this pipeline's directory and an artifact entry against the step's workspace, so one list cannot hold both", label)
	}

	return nil
}

// applyFixInclude resolves a fix:'s message_files:, shared by a task step's own
// fix: and a tasks: entry's.
func applyFixInclude(resolver *includeResolver, context string, fix *FixSpec) error {
	if fix == nil || len(fix.MessageFiles) == 0 {
		return nil
	}

	if len(fix.Messages) > 0 {
		return fmt.Errorf("%s fix: messages: and message_files: are mutually exclusive — two ordered lists cannot say which message comes first", context)
	}

	messages := make([]string, 0, len(fix.MessageFiles))

	for _, path := range fix.MessageFiles {
		var message string

		err := applyInclude(resolver, include{context: context + " fix", key: "messages", path: path, target: &message})
		if err != nil {
			return err
		}

		messages = append(messages, message)
	}

	fix.Messages = messages
	fix.MessageFiles = nil

	return nil
}
