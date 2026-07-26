package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
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
// The one exception is an agent step's prompt_file: {artifact, path} mapping
// form (see FileRef.Deferred): that names a file inside an artifact a get
// step fetches, which does not exist yet at load time, so it is left
// untouched here and resolved later by internal/agent, once the artifact is
// on disk.
func (c *Config) resolveFileIncludes(baseDir string) error {
	for i := range c.Tasks {
		err := resolveTaskIncludes(baseDir, &c.Tasks[i])
		if err != nil {
			return err
		}
	}

	for i := range c.Agents {
		err := resolveAgentIncludes(baseDir, &c.Agents[i])
		if err != nil {
			return err
		}
	}

	for i := range c.Jobs {
		err := c.Jobs[i].visitSteps(func(label string, step *Step) error {
			return resolveStepIncludes(baseDir, label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
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
func applyInclude(baseDir string, inc include) error {
	if inc.path == "" {
		return nil
	}

	if *inc.target != "" {
		return fmt.Errorf("%s: %s: and %s_file: are mutually exclusive", inc.context, inc.key, inc.key)
	}

	data, err := readIncludeFile(baseDir, inc.context, inc.key+"_file", inc.path)
	if err != nil {
		return err
	}

	*inc.target = string(data)

	return nil
}

// readIncludeFile resolves path relative to baseDir and reads it. A path may
// use ".." to escape baseDir: the pipeline file is trusted input (see
// LoadConfig's own os.ReadFile), and a file placed beside it by the same
// author is at the same trust level — a shared ../tasks/ directory next to a
// pipelines/ directory is a legitimate layout, not a hole to close.
//
// A not-found error carries a specific hint for the common Concourse-habit
// mistake (run_file: repo/ci/build.sh, meaning a fetched artifact): that path
// essentially never exists next to a pipeline YAML, so this fires in
// practice for exactly that case.
func readIncludeFile(baseDir, context, key, path string) ([]byte, error) {
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

	return data, nil
}

// resolveTaskIncludes applies a tasks: entry's file: (a whole-document
// include, merged so the entry's own inline fields win) and its run_file:/
// fix.prompt_file:.
func resolveTaskIncludes(baseDir string, t *Task) error {
	context := fmt.Sprintf("task %q", t.Name)

	if t.File != "" {
		doc, err := loadTaskDocument(baseDir, context, t.File)
		if err != nil {
			return err
		}

		mergeTaskDocument(t, doc)
	}

	err := applyInclude(baseDir, include{context: context, key: "run", path: t.RunFile, target: &t.Run})
	if err != nil {
		return err
	}

	return applyFixInclude(baseDir, context, t.Fix)
}

// loadTaskDocument reads and parses path as a standalone Task document. The
// document may not itself use file:/run_file: — includes are resolved one
// level deep only, which is what makes cycle detection unnecessary.
func loadTaskDocument(baseDir, context, path string) (Task, error) {
	data, err := readIncludeFile(baseDir, context, "file", path)
	if err != nil {
		return Task{}, err
	}

	var doc Task

	err = yaml.Unmarshal(data, &doc)
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
func resolveAgentIncludes(baseDir string, a *Agent) error {
	context := fmt.Sprintf("agent %q", a.Name)

	if a.File != "" {
		doc, err := loadAgentDocument(baseDir, context, a.File)
		if err != nil {
			return err
		}

		mergeAgentDocument(a, doc)
	}

	return applyInclude(baseDir, include{context: context, key: "system", path: a.SystemFile, target: &a.System})
}

// loadAgentDocument reads and parses path as a standalone Agent document. The
// document may not itself use file:/system_file: — includes are resolved one
// level deep only.
func loadAgentDocument(baseDir, context, path string) (Agent, error) {
	data, err := readIncludeFile(baseDir, context, "file", path)
	if err != nil {
		return Agent{}, err
	}

	var doc Agent

	err = yaml.Unmarshal(data, &doc)
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
// touched: the entry, not the document, names the agent. Split into two
// halves purely to stay under the linter's cyclomatic-complexity budget —
// there is no grouping significance to the split.
func mergeAgentDocument(a *Agent, doc Agent) {
	mergeAgentIdentity(a, doc)
	mergeAgentDials(a, doc)
}

func mergeAgentIdentity(a *Agent, doc Agent) {
	if a.Source == (AgentSource{}) {
		a.Source = doc.Source
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

	if a.MaxTurns == 0 {
		a.MaxTurns = doc.MaxTurns
	}

	if a.CompactAfterTokens == nil {
		a.CompactAfterTokens = doc.CompactAfterTokens
	}
}

// resolveStepIncludes applies one step's own includes: run_file: (task steps
// only), prompt_file: when it's the load-time scalar form (agent steps only —
// the {artifact, path} mapping form is deferred to run time; see FileRef and
// internal/agent), and its fix:'s prompt_file:.
func resolveStepIncludes(baseDir, label string, step *Step) error {
	if step.RunFile != "" && step.Task == "" {
		return fmt.Errorf("%s: run_file: is only valid on task steps", label)
	}

	err := applyInclude(baseDir, include{context: label, key: "run", path: step.RunFile, target: &step.Run})
	if err != nil {
		return err
	}

	if step.PromptFile != nil && step.Agent == "" {
		return fmt.Errorf("%s: prompt_file: is only valid on agent steps", label)
	}

	if step.PromptFile != nil && !step.PromptFile.Deferred() {
		if step.Prompt != "" {
			return fmt.Errorf("%s: prompt: and prompt_file: are mutually exclusive", label)
		}

		data, err := readIncludeFile(baseDir, label, "prompt_file", step.PromptFile.Path)
		if err != nil {
			return err
		}

		step.Prompt = string(data)
		step.PromptFile = nil
	}

	return applyFixInclude(baseDir, label, step.Fix)
}

// applyFixInclude resolves a fix:'s prompt_file:, shared by a task step's own
// fix: and a tasks: entry's.
func applyFixInclude(baseDir, context string, fix *FixSpec) error {
	if fix == nil {
		return nil
	}

	return applyInclude(baseDir, include{context: context + " fix", key: "prompt", path: fix.PromptFile, target: &fix.Prompt})
}
