package config

// ((var)) interpolation, ((.:name)) references to what a run captured, and the
// load_var: step.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// varPattern matches a ((name)) reference. Concourse's spelling, kept because
// a pipeline moving between the two should not have to be rewritten.
var varPattern = regexp.MustCompile(`\(\(([a-zA-Z0-9_.\-]+)\)\)`) //nolint:gochecknoglobals // compiled once, read-only

// localVarPattern matches a ((.:name)) or ((.:name.field...)) reference:
// Concourse's spelling for a build-local var, which here is a load_var:'s
// value or a get's facts. varPattern never matches one, since ':' is not in
// its alphabet, so --var substitution leaves these for the run.
var localVarPattern = regexp.MustCompile(`\(\(\.:([a-zA-Z0-9_\-]+(?:\.[a-zA-Z0-9_\-]+)*)\)\)`) //nolint:gochecknoglobals // compiled once, read-only

const localVarOpen = "((.:"

// InterpolateVars substitutes ((name)) throughout a pipeline's YAML source,
// before it is parsed.
//
// Textual, not structural, and deliberately so: a var may appear anywhere a
// value does — inside a URI, in the middle of a command, as a whole mapping
// value — and a structural pass would have to enumerate every field that might
// contain one, which is the same list that goes stale every time a field is
// added.
//
// Unknown references are LEFT IN PLACE rather than blanked: blanking would
// turn a mistake into a silently empty string in a command, where leaving it
// lets validateVars name it.
func InterpolateVars(source []byte, vars map[string]string) []byte {
	if len(vars) == 0 {
		return source
	}

	return varPattern.ReplaceAllFunc(source, func(match []byte) []byte {
		name := varPattern.FindSubmatch(match)[1]

		value, ok := vars[string(name)]
		if !ok {
			return match
		}

		return []byte(value)
	})
}

// UnresolvedVars lists every ((name)) still present in a value, sorted.
func UnresolvedVars(value string) []string {
	matches := varPattern.FindAllStringSubmatch(value, -1)

	seen := map[string]bool{}
	names := make([]string, 0, len(matches))

	for _, match := range matches {
		if !seen[match[1]] {
			seen[match[1]] = true
			names = append(names, match[1])
		}
	}

	slices.Sort(names)

	return names
}

// RenderVars substitutes ((name)) in a single value from a var map, leaving
// unknown references in place.
func RenderVars(value string, vars map[string]string) string {
	if value == "" || !strings.Contains(value, "((") {
		return value
	}

	return string(InterpolateVars([]byte(value), vars))
}

// LocalRef is one ((.:root.field...)) reference.
type LocalRef struct {
	Root   string
	Fields []string
}

func (r LocalRef) String() string {
	return localVarOpen + strings.Join(append([]string{r.Root}, r.Fields...), ".") + "))"
}

func parseLocalRef(path string) LocalRef {
	parts := strings.Split(path, ".")

	return LocalRef{Root: parts[0], Fields: parts[1:]}
}

// LocalRefs lists every well-formed ((.:name)) reference in a value, in order.
func LocalRefs(value string) []LocalRef {
	if !strings.Contains(value, localVarOpen) {
		return nil
	}

	matches := localVarPattern.FindAllStringSubmatch(value, -1)
	refs := make([]LocalRef, 0, len(matches))

	for _, match := range matches {
		refs = append(refs, parseLocalRef(match[1]))
	}

	return refs
}

// malformedLocalRef is the first ((.: that does not open a well-formed
// reference, or "". Without it a typo reaches a shell as literal text.
func malformedLocalRef(value string) string {
	for rest := value; ; {
		at := strings.Index(rest, localVarOpen)
		if at < 0 {
			return ""
		}

		rest = rest[at:]

		loc := localVarPattern.FindStringIndex(rest)
		if loc == nil || loc[0] != 0 {
			end, _, _ := strings.Cut(rest, "))")

			return end + "))"
		}

		rest = rest[loc[1]:]
	}
}

// withoutLocalRefs stands a plain word in for each ((.:name)), for a load-time
// check of the text around a value only the run will know: the reference's own
// ':' is not the author's.
func withoutLocalRefs(text string) string {
	return localVarPattern.ReplaceAllString(text, "v")
}

// RenderLocalVars substitutes every ((.:name)) in text with what lookup
// answers. A rendered value is never scanned again, so a captured value that
// itself spells a reference stays text.
func RenderLocalVars(text string, lookup func(LocalRef) (string, error)) (string, error) {
	if !strings.Contains(text, localVarOpen) {
		return text, nil
	}

	var firstErr error

	out := localVarPattern.ReplaceAllStringFunc(text, func(match string) string {
		value, err := lookup(parseLocalRef(localVarPattern.FindStringSubmatch(match)[1]))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}

			return match
		}

		return value
	})

	return out, firstErr
}

// validateVars rejects a ((name)) that nothing will ever supply, a ((.:name))
// no earlier step captured, and a malformed load_var: step.
//
// Catching it at load is the whole point: an unresolved var otherwise reaches
// a shell command as the literal text `((repo_uri))`, which fails somewhere
// far from the mistake.
//
// A ((.:name)) is a load_var:'s value or a get's facts, and counts as produced
// only where the run GUARANTEES it was captured, because anywhere else the
// literal text is exactly what arrives:
//   - sequential steps (the plan, a do: block) see what ran before them;
//   - concurrent branches see only what preceded the block, since whether one
//     branch sees another's capture is a race; after the block, an
//     in_parallel: (which succeeds only when every branch did) yields every
//     branch's captures, while a race: or ensemble: yields only those common
//     to all branches, since any one of them may be the one that won;
//   - a try: body's captures stay inside it, since the try succeeds whether or
//     not the body did;
//   - a step with a when: guard yields nothing it captured, itself or nested:
//     a false guard skips it green, so the plan goes on without the capture.
//     Its own on_success still sees them, since a skipped step fires no hooks.
//     Guard a do: around the load_var: and its consumers instead, so the
//     consumers are skipped with it;
//   - an across: step's captures stay in the cell that made them, Concourse's
//     scope for a cell;
//   - hooks see what preceded the step, and on_success also what the step
//     captured; nothing a hook captures escapes it.
func (c *Config) validateVars() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(validateLoadVarStep)
		if err != nil {
			return err
		}

		flow, err := newVarFlow(job)
		if err != nil {
			return err
		}

		produced := varSet{}

		for i := range job.Plan {
			produced, err = flow.check(job.stepLabel(i), &job.Plan[i], produced)
			if err != nil {
				return err
			}
		}

		err = flow.hooks(job.Hooks, varSet{}, produced, func(name string) string {
			return fmt.Sprintf("%s %s hook", job.label(), name)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

type varSet map[string]bool

func (s varSet) union(other varSet) varSet {
	out := make(varSet, len(s)+len(other))
	maps.Copy(out, s)
	maps.Copy(out, other)

	return out
}

func (s varSet) with(names ...string) varSet {
	out := make(varSet, len(s)+len(names))
	for name := range s {
		out[name] = true
	}

	for _, name := range names {
		out[name] = true
	}

	return out
}

// varFlow is what a job's checks need to word an error: the names a
// load_var:, a get or a put anywhere in the job goes by.
type varFlow struct {
	loadVars, gets, puts varSet
}

// newVarFlow names a job's captures and refuses two of one name in one scope:
// the job's plan is one scope, and each across: step's body another, as each
// cell's values are its own.
func newVarFlow(job Job) (*varFlow, error) {
	flow := &varFlow{loadVars: varSet{}, gets: varSet{}, puts: varSet{}}

	scopes := func(_ string, step *Step, scope map[string]string) (map[string]string, map[string]string) {
		if len(step.Across) > 0 {
			return map[string]string{}, scope
		}

		return scope, scope
	}

	err := walkJob(job, map[string]string{}, scopes, flow.name)

	return flow, err
}

// nameCapture files a load_var:'s or an ask:'s name, refusing one the scope already has: a free-text ask: captures nothing, but its artifact is named too.
func (f *varFlow) nameCapture(label string, step *Step, scope map[string]string) error {
	kind, name := "load_var", step.LoadVar
	if step.Ask != "" {
		kind, name = "ask", step.Ask
	}

	f.loadVars[name] = true

	if earlier, taken := scope[name]; taken {
		return fmt.Errorf("%s: %s %q is already the name of %s in this plan; a ((.:%s)) names one value, so give each capture its own name",
			label, kind, name, earlier, name)
	}

	scope[name] = "an earlier " + kind + ":"

	return nil
}

// name files what a step names in its scope, refusing a capture whose name
// the scope already has.
func (f *varFlow) name(label string, step *Step, scope map[string]string) error {
	switch kind, _ := step.Kind(); kind {
	case StepKindLoadVar, StepKindAsk:
		return f.nameCapture(label, step, scope)
	case StepKindGet:
		f.gets[step.Get] = true

		if earlier, taken := scope[step.Get]; taken && earlier != "a get" {
			return fmt.Errorf("%s: get %q is already the name of %s in this plan; ((.:%s)) would name both, so rename one", label, step.Get, earlier, step.Get)
		}

		scope[step.Get] = "a get"
	case StepKindPut:
		f.puts[step.Put] = true
	case StepKindTask, StepKindAgent, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindApproval, StepKindDo:
		// Captures nothing and names nothing a ((.:name)) could mean.
	}

	return nil
}

// check checks one step and everything nested in it against the captures
// available when it starts, and returns those available once it has
// succeeded. Labels follow walkStepTree's, so an error names the same place
// every other validator would.
func (f *varFlow) check(label string, step *Step, before varSet) (varSet, error) {
	err := f.checkStep(label, step, before)
	if err != nil {
		return nil, err
	}

	after, err := f.nested(label, step, before, before.with(ownCaptures(step)...))
	if err != nil {
		return nil, err
	}

	// Unguarded captures: a guard-skipped step fires no hooks, so a running on_success ran after them.
	err = f.hooks(step.Hooks, before, after, func(name string) string {
		return fmt.Sprintf("%s (%s hook)", label, name)
	})
	if err != nil {
		return nil, err
	}

	// A false when: skips the step green, so its captures are not guaranteed past it (#262); a cell's are its own.
	if step.Guarded() || len(step.Across) > 0 {
		return before, nil
	}

	return after, nil
}

// ownCaptures is the ((.:name)) roots a step itself produces.
func ownCaptures(step *Step) []string {
	switch kind, _ := step.Kind(); kind {
	case StepKindLoadVar:
		return []string{step.LoadVar}
	case StepKindGet:
		return []string{step.Get}
	case StepKindAsk:
		// Free text is never captured: a person's words substituted into a run: would be a shell command they wrote.
		if len(step.Options) > 0 {
			return []string{step.Ask}
		}
	case StepKindTask, StepKindPut, StepKindAgent, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindApproval, StepKindDo:
		// A put's facts are not a later step's to read, as in Concourse.
	}

	return nil
}

// nested checks what a step holds — a try: body, a block's branches, a do:'s
// steps — and returns what is available once they have succeeded.
func (f *varFlow) nested(label string, step *Step, before, after varSet) (varSet, error) {
	if step.Try != nil {
		_, err := f.check(label+" (try)", step.Try, before)
		if err != nil {
			return nil, err
		}
	}

	after, err := f.branches(label, step, before, after)
	if err != nil {
		return nil, err
	}

	for i := range step.Do {
		after, err = f.check(fmt.Sprintf("%s (do step %d)", label, i), &step.Do[i], after)
		if err != nil {
			return nil, err
		}
	}

	return after, nil
}

// branches checks a concurrent block's branches, each from what preceded the
// block, and adds what the block yields once it has succeeded to after.
func (f *varFlow) branches(label string, step *Step, before, after varSet) (varSet, error) {
	kind, branches := branchesOf(step)

	var joined varSet

	for i, branch := range branches {
		out, err := f.check(fmt.Sprintf("%s (%s branch %d)", label, kind, i), branch, before)
		if err != nil {
			return nil, err
		}

		joined = joinBranch(kind, joined, out, i == 0)
	}

	return after.union(joined), nil
}

// hooks checks hooks against what preceded their step, giving on_success
// alone what the step captured.
func (f *varFlow) hooks(hooks Hooks, before, after varSet, label func(name string) string) error {
	return hooks.Each(func(name string, hook *Step) error {
		seen := before

		switch name {
		case "on_success":
			seen = after
		case "on_pending":
			if before[PendingCapture] {
				return fmt.Errorf("%s: a capture named %q would be hidden by the park's ((.:%s.*)) here; rename it", label(name), PendingCapture, PendingCapture)
			}

			seen = before.with(PendingCapture)
		}

		_, err := f.check(label(name), hook, seen)

		return err
	})
}

// joinBranch folds one branch's captures into what its block yields: all of
// them for in_parallel:, only the common ones for a block whose outcome is
// whichever branch won.
func joinBranch(kind StepKind, joined, branch varSet, first bool) varSet {
	if first {
		return branch
	}

	if kind == StepKindInParallel {
		return branch.union(joined)
	}

	common := varSet{}

	for name := range joined {
		if branch[name] {
			common[name] = true
		}
	}

	return common
}

// The formats a load_var: reads its file in. Concourse also spells yaml as
// yml, which LoadVarFormat folds into LoadVarYAML.
const (
	LoadVarJSON = "json"
	LoadVarYAML = "yaml"
	LoadVarTrim = "trim"
	LoadVarRaw  = "raw"
)

func loadVarFormat(spelling string) (string, bool) {
	switch spelling {
	case "json":
		return LoadVarJSON, true
	case "yaml", "yml":
		return LoadVarYAML, true
	case "trim":
		return LoadVarTrim, true
	case "raw":
		return LoadVarRaw, true
	}

	return "", false
}

// LoadVarFormat is how a load_var: step reads its file: format: when set,
// otherwise the file's extension when it names a format (Concourse's
// fileFormat, so .trim and .raw count too), otherwise trim.
func (s Step) LoadVarFormat() string {
	if format, ok := loadVarFormat(s.Format); ok {
		return format
	}

	if format, ok := loadVarFormat(strings.TrimPrefix(path.Ext(s.VarFile), ".")); ok {
		return format
	}

	return LoadVarTrim
}

// validateLoadVarStep checks a load_var: step names a var and a file, and that
// its format:, if any, is one Concourse reads.
func validateLoadVarStep(label string, step *Step) error {
	if step.LoadVar == "" {
		return refuseLoadVarFields(label, step)
	}

	if step.VarFile == "" {
		return fmt.Errorf("%s: load_var %q needs a file: to read the value from", label, step.LoadVar)
	}

	if _, ok := loadVarFormat(step.Format); step.Format != "" && !ok {
		return fmt.Errorf("%s: load_var %q format %q is not one of json, yaml, yml, trim or raw", label, step.LoadVar, step.Format)
	}

	// The read happens on the orchestrator host, so a path leaving the step's
	// workspace names a file the pipeline does not own. One still carrying a
	// var is checked by the run once rendered.
	if len(UnresolvedVars(step.VarFile)) > 0 || strings.Contains(step.VarFile, localVarOpen) {
		return nil
	}

	if _, ok := ConfinedPath(".", step.VarFile); !ok {
		return fmt.Errorf("%s: load_var %q file %q escapes the workspace; it must be a path inside a declared input, like meta/version.txt", label, step.LoadVar, step.VarFile)
	}

	return nil
}

// refuseLoadVarFields refuses a load_var: field on a step that is not one.
func refuseLoadVarFields(label string, step *Step) error {
	switch {
	case step.VarFile != "":
		return fmt.Errorf("%s: file: without load_var: has no meaning here", label)
	case step.Format != "":
		return fmt.Errorf("%s: format: without load_var: has no meaning here", label)
	case step.Reveal:
		return fmt.Errorf("%s: reveal: without load_var: has no meaning here", label)
	}

	return nil
}

// checkStep rejects references a step makes to vars nothing supplies.
//
// The field list must match what Step.RenderLocalVars actually substitutes,
// or a reference passes validation and then reaches the outside world
// verbatim: a `((version))` in a put's params: was neither rejected here nor
// rendered there, so the resource's out: published a release literally named
// "((version))".
func (f *varFlow) checkStep(label string, step *Step, produced varSet) error {
	for _, field := range varFields(step) {
		err := f.checkText(label, field.name, *field.value, produced)
		if err != nil {
			return err
		}
	}

	// params: is a free-form mapping rather than a field, so it is walked
	// rather than listed.
	for _, text := range stringsInValue(step.Params) {
		err := f.checkText(label, "params", text, produced)
		if err != nil {
			return err
		}

		// A get's params are part of which fetch it is, settled before the plan runs; only a step that runs in place is rendered.
		if step.Get != "" && strings.Contains(text, localVarOpen) {
			return fmt.Errorf("%s: get %q params has %q; a get's params are not filled in at run time, so a ((.:name)) there would reach the resource as that text", label, step.Get, text)
		}
	}

	return nil
}

// checkText checks one value's ((name)) and ((.:name)) references.
func (f *varFlow) checkText(label, field, text string, produced varSet) error {
	if names := UnresolvedVars(text); len(names) > 0 {
		name := names[0]
		root, _, _ := strings.Cut(name, ".")

		// A --var may share a get's name, so this says both rather than guessing which was meant.
		if f.loadVars[root] || f.gets[root] {
			return fmt.Errorf("%s: %s references ((%s)), which is not passed with --var/--vars-file; a value the run captured is read as ((.:%s)), Concourse's spelling for it", label, field, name, name)
		}

		return fmt.Errorf("%s: %s references ((%s)), which is not passed with --var/--vars-file", label, field, name)
	}

	return f.checkLocalRefs(label, field, text, produced)
}

// checkLocalRefs checks one value's ((.:name)) references against what is produced.
func (f *varFlow) checkLocalRefs(label, field, text string, produced varSet) error {
	if bad := malformedLocalRef(text); bad != "" {
		return fmt.Errorf("%s: %s has %q, which is not a ((.:name)) or ((.:name.field)) reference", label, field, bad)
	}

	for _, ref := range LocalRefs(text) {
		if ref.Root == PendingCapture {
			err := checkPendingRef(label, field, ref, produced)
			if err != nil {
				return err
			}

			continue
		}

		switch {
		case produced[ref.Root]:
			continue
		case f.puts[ref.Root] && !f.gets[ref.Root] && !f.loadVars[ref.Root]:
			return fmt.Errorf("%s: %s references %s, but a put's facts are not available to later steps (as in Concourse, only a get's are)", label, field, ref)
		default:
			return fmt.Errorf("%s: %s references %s, which no load_var: or get: earlier in this plan produces", label, field, ref)
		}
	}

	return nil
}

// varField is one string field a var may appear in, addressed in place so
// RenderLocalVars can substitute through it.
type varField struct {
	name  string
	value *string
}

// varFields lists a step's substitutable string fields, so validation
// (checkStep) and run-time rendering (Step.RenderLocalVars) cannot drift
// apart. Adding a field here is what makes it both checked and rendered.
func varFields(step *Step) []varField {
	fields := make([]varField, 0, 4+len(step.Messages))
	fields = append(fields,
		varField{"run", &step.Run},
		varField{"image", &step.Image},
		varField{"dir", &step.Dir},
		varField{"file", &step.VarFile},
		varField{"question", &step.Question},
	)

	for i := range step.Messages {
		fields = append(fields, varField{fmt.Sprintf("messages[%d]", i), &step.Messages[i]})
	}

	// A release's approval names the version it would publish.
	if step.Approval != nil {
		fields = append(fields, varField{"approval.message", &step.Approval.Message})
	}

	return fields
}

// LocalRefs lists the ((.:name)) references a step's own fields make — not
// those of the steps nested in it.
func (s Step) LocalRefs() []LocalRef {
	fields := varFields(&s)
	refs := make([]LocalRef, 0, len(fields))

	for _, field := range fields {
		refs = append(refs, LocalRefs(*field.value)...)
	}

	for _, text := range stringsInValue(s.Params) {
		refs = append(refs, LocalRefs(text)...)
	}

	return refs
}

// RenderLocalVars returns s with every ((.:name)) in its var fields and
// params: substituted from lookup, failing on the first reference lookup
// cannot answer.
//
// Slices are cloned before rendering, never written through: s is a copy of
// the config's step, but a slice field still points at the config's own
// array — so rendering through it would substitute into the LOADED pipeline,
// and a long-lived process (steps web) hands that same config to every run.
// The second run would then find no placeholder left, send the first run's
// values, and — because the substituted text is what gets hashed — hash
// identically to it: a false cache hit.
func (s Step) RenderLocalVars(lookup func(LocalRef) (string, error)) (Step, error) {
	if len(s.Messages) > 0 {
		s.Messages = slices.Clone(s.Messages)
	}

	if s.Approval != nil {
		approval := *s.Approval
		s.Approval = &approval
	}

	for _, field := range varFields(&s) {
		rendered, err := RenderLocalVars(*field.value, lookup)
		if err != nil {
			return s, fmt.Errorf("%s: %w", field.name, err)
		}

		*field.value = rendered
	}

	// params: is where a captured version most often belongs — it is what a
	// put hands the resource. The literal text `((version))` used to reach the
	// out: command and publish a release by that name.
	rendered, err := mapStringLeaves("params", s.Params, func(_, text string) (string, error) { return RenderLocalVars(text, lookup) })
	if err != nil {
		return s, fmt.Errorf("params: %w", err)
	}

	s.Params, _ = rendered.(map[string]any)
	// A put's cache key carries params: even when empty, and has always been handed {} here, never null.
	if s.Params == nil {
		s.Params = map[string]any{}
	}

	return s, nil
}

// stringsInValue walks a decoded YAML value — a params: mapping, a nested
// list, a scalar — and returns every string in it.
func stringsInValue(value any) []string {
	var texts []string

	_, _ = mapStringLeaves("", value, func(_, leaf string) (string, error) {
		texts = append(texts, leaf)

		return leaf, nil
	})

	return texts
}

// ParseLoadVar reads a load_var: file's bytes in format: json and yaml as
// structured values whose fields ((.:name.field)) reaches, trim with its
// surrounding whitespace dropped — `git describe > version.txt` leaves a
// newline that would otherwise land mid-command — and raw exactly as written.
func ParseLoadVar(format string, body []byte) (any, error) {
	switch format {
	case LoadVarJSON:
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()

		var value any

		err := decoder.Decode(&value)
		if err != nil {
			return nil, fmt.Errorf("read as json: %w", err)
		}

		// More() answers false before a stray } or ], so only EOF proves the file held one value.
		_, err = decoder.Token()
		if !errors.Is(err, io.EOF) {
			return nil, errors.New("read as json: more than one value")
		}

		return value, nil
	case LoadVarYAML:
		var value any

		err := yaml.Unmarshal(body, &value)
		if err != nil {
			return nil, fmt.Errorf("read as yaml: %w", err)
		}

		return stringKeys(value), nil
	case LoadVarRaw:
		return string(body), nil
	default:
		return strings.TrimSpace(string(body)), nil
	}
}

// LocalValue is the text ref names inside value: its fields followed through
// mappings, a string as itself and anything else as JSON — a number, true,
// null, or a whole mapping or list.
func LocalValue(value any, ref LocalRef) (string, error) {
	for i, field := range ref.Fields {
		fields, ok := value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%s: %s has no field %q; it is %s", ref, LocalRef{Root: ref.Root, Fields: ref.Fields[:i]}, field, describeLocal(value))
		}

		value, ok = fields[field]
		if !ok {
			return "", fmt.Errorf("%s: %s has no field %q", ref, LocalRef{Root: ref.Root, Fields: ref.Fields[:i]}, field)
		}
	}

	if text, ok := value.(string); ok {
		return text, nil
	}

	// Not json.Marshal: it writes & < > as \u0026 \u003c \u003e, which is not the text captured, and a masked string would no longer match it.
	var encoded strings.Builder

	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)

	err := encoder.Encode(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}

	return strings.TrimSuffix(encoded.String(), "\n"), nil
}

// stringKeys spells every mapping key of a decoded YAML value as a string, as
// Concourse's YAML-to-JSON reading does: yaml.v3 decodes a mapping with any
// other key as map[any]any, which no field path, mask or encoder reaches.
func stringKeys(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			typed[key] = stringKeys(nested)
		}

		return typed
	case map[any]any:
		return stringKeys(StringKeys(typed))
	case []any:
		for i, nested := range typed {
			typed[i] = stringKeys(nested)
		}

		return typed
	default:
		return value
	}
}

func describeLocal(value any) string {
	switch value.(type) {
	case string:
		return "plain text"
	case []any:
		return "a list"
	default:
		return "not a mapping"
	}
}

// LocalStrings is every string in a captured value: what masking hides of it.
// A number or a boolean is not hidden, as Concourse's tracker hides only
// strings — masking every 1 a run prints would hide nothing and garble all.
func LocalStrings(value any) []string {
	return stringsInValue(value)
}
