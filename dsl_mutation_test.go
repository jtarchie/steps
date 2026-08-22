package main

// Mutation testing for the DSL itself.
//
// TestAssertMutation proves the corpus's assertions are not vacuous. It does
// not prove they are SENSITIVE to the feature each example teaches: an
// example demonstrating input_mapping: whose only assertion is "the job ran"
// would survive input_mapping: being deleted from the language.
//
// So mutate the PIPELINE rather than the expectation, one config.Step field
// at a time, and require the build to notice. What counts as noticing is
// recorded per field, because the three modes are not equally good:
//
//	load    `steps validate` rejected the mutant — the field is validated
//	assert  a doc assertion caught it — the field's BEHAVIOR is pinned
//	crash   the run broke some other way — real, but incidental coverage
//
// A `crash` detection is a hint that the example asserting that field is
// thinner than it looks; the summary line prints the tally so those stay
// visible rather than reading as full coverage.
//
// TestDSLMutationCoversStep is the ratchet: every yaml tag on config.Step
// must have an operator here or a written reason in stepMutationSkips. That
// makes a new DSL field cost a schema entry (schema_test.go), a doc example
// (TestDocsCoverage), AND either a way to break it or an argument for why it
// cannot be broken.

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/docs"
	"github.com/jtarchie/steps/internal/config"
)

// stepOperator breaks one config.Step field. apply mutates the step in place
// and reports whether it found something to change — a field whose value has
// no falsifiable form in this particular step (an empty list, a one-entry
// vocabulary) reports false and the walk moves on to the next site.
type stepOperator struct {
	tag   string
	apply func(step map[string]any) bool
}

// stepMutationSkips are the config.Step fields with no operator, each with
// the reason. A reason is mandatory for the same rule kindswitch applies to
// its own ignores: an unexplained omission is indistinguishable from an
// oversight, and this is exactly the list where an oversight hides.
var stepMutationSkips = map[string]string{ //nolint:gochecknoglobals // a test's data table
	"trigger":  "steps test runs every job unconditionally, so trigger: changes nothing it can see; it decides polling under steps watch, covered by internal/trigger",
	"approval": "an approval parks the run until a person answers, which is why its example is noexec=approval — there is no run to mutate",
	"on_abort": "only SIGINT/SIGTERM mid-run reaches it, and a fixture that signals itself is testing the harness; pinned by TestConformanceAbortFiresOnAbortHook and TestRunHooksAbortGracePeriod instead",

	"image":            "needs a docker daemon (STEPS_TEST_DOCKER=1); every example using it is noexec=docker",
	"env":              "same: only spelled in noexec=docker blocks, since the point is what a containerized command inherits",
	"user":             "same: noexec=docker",
	"network":          "same: noexec=docker",
	"privileged":       "same: noexec=docker",
	"container_limits": "same: noexec=docker",

	"prompt":            "the scripted provider answers by POSITION, not by prompt content, so rewriting a prompt changes nothing a fixture can observe — asserting on it would be asserting on the fake",
	"max_context_bytes": "same reason: it bounds how much context_paths content the model is handed, and a positional fake says the same thing however much it receives",

	"max_in_flight": "documented to change only how many cells run at once, never which run or in what order (it is deliberately not even hashed) — there is nothing for an assertion to see",
	"volatile":      "it decides whether the step cache may reuse a step, and that cache exists only under a durable workspace.root: across two runs — a corpus of single-run fixtures in a fresh temp dir has no cache for it to change; pinned by TestStepCacheReusesAnAgentStep and internal/merkle's TestStepCacheable instead",

	"assert": "mutating an assertion is TestAssertMutation's whole job; doing it here too would test the same 215 mutants twice",
}

// stepOperators is one way to break each remaining config.Step field.
//
//nolint:gochecknoglobals // a test's data table
var stepOperators = []stepOperator{
	{tag: "get", apply: setString("get", mutantMarker)},
	{tag: "put", apply: setString("put", mutantMarker)},
	{tag: "task", apply: setString("task", mutantMarker)},
	{tag: "agent", apply: setString("agent", mutantMarker)},
	{tag: "resource", apply: setString("resource", mutantMarker)},
	{tag: "load_var", apply: setString("load_var", mutantMarker)},
	{tag: "dir", apply: setString("dir", mutantMarker)},
	{tag: "run_file", apply: setString("run_file", mutantMarker)},
	{tag: "file", apply: setString("file", mutantMarker)},

	// A command that runs fine and says something else: the assertion on
	// what it printed is the only thing between this and a green build.
	{tag: "run", apply: setString("run", "echo "+mutantMarker)},

	{tag: "version", apply: mutateVersion},
	{tag: "params", apply: mutateFirstMapValue("params")},
	{tag: "input_mapping", apply: mutateFirstMapValue("input_mapping")},
	{tag: "output_mapping", apply: mutateFirstMapValue("output_mapping")},

	{tag: "prompt_file", apply: mutatePromptFile},
	{tag: "context_paths", apply: replaceFirstOfList("context_paths", mutantMarker)},
	{tag: "inputs", apply: dropFirstOfList("inputs")},
	{tag: "outputs", apply: dropFirstOfList("outputs")},
	{tag: "passed", apply: replaceFirstOfList("passed", mutantMarker)},
	{tag: "tools", apply: narrowToolsToReadFile},

	// Remove the repair and the task simply stays broken.
	{tag: "fix", apply: deleteKey("fix")},
	// Remove the decision the reader was handed and its consumer has
	// nothing to read.
	{tag: "context", apply: deleteKey("context")},

	{tag: "when", apply: setString("when", "false")},
	{tag: "to", apply: mutateRouteTargets("to")},
	{tag: "verdicts", apply: mutateVerdictEntries},
	{tag: "max_visits", apply: setInt("max_visits", 1)},

	{tag: "attempts", apply: setInt("attempts", 1)},
	{tag: "max_turns", apply: setInt("max_turns", 1)},
	{tag: "timeout", apply: setString("timeout", "1ms")},
	// Delete the placement and the step runs here instead, so STEPS_WORKER is
	// unset and the example's own stdout assertion goes red. The falsifiable
	// claim is "it ran somewhere else", which is the whole of what tags: means.
	// Rewriting the tag instead would be caught too, but as a crash — an
	// unmapped tag is refused before the run starts — and a caught assertion
	// is the stronger evidence.
	{tag: "tags", apply: deleteKey("tags")},

	// Raising a matrix's allowance admits the cells it was stopping.
	{tag: "budget", apply: scaleBudgetTokens},
	// A narrower matrix is a shorter execution log.
	{tag: "across", apply: truncateAcross},

	// Each container loses a branch, so its children no longer match.
	{tag: "do", apply: truncateList("do")},
	{tag: "in_parallel", apply: truncateBranches("in_parallel", "steps")},
	{tag: "race", apply: truncateBranches("race", "steps")},
	{tag: "ensemble", apply: truncateBranches("ensemble", "agents")},

	// Unwrap the tolerance and the failure it hides stops the plan.
	{tag: "try", apply: unwrapTry},

	// A hook that stops firing is a name missing from the execution log,
	// which is the whole reason hooks are recorded there.
	{tag: "on_success", apply: deleteKey("on_success")},
	{tag: "on_failure", apply: deleteKey("on_failure")},
	{tag: "on_error", apply: deleteKey("on_error")},
	{tag: "ensure", apply: deleteKey("ensure")},
}

// TestDSLMutation breaks each config.Step field once and requires the corpus
// to notice.
func TestDSLMutation(t *testing.T) {
	blocks := runnableBlocks(t)

	modes := map[string]string{}

	for _, operator := range stepOperators {
		t.Run(operator.tag, func(t *testing.T) {
			modes[operator.tag] = detectFieldSomewhere(t, blocks, operator)
		})
	}

	reportDetectionModes(t, modes)
}

// reportDetectionModes prints the tally, naming the fields whose only
// coverage is incidental.
func reportDetectionModes(t *testing.T, modes map[string]string) {
	t.Helper()

	byMode := map[string][]string{}
	for tag, mode := range modes {
		byMode[mode] = append(byMode[mode], tag)
	}

	for _, mode := range []string{"assert", "load", "crash"} {
		tags := byMode[mode]
		sort.Strings(tags)

		if len(tags) > 0 {
			t.Logf("caught by %-6s (%2d): %s", mode, len(tags), strings.Join(tags, " "))
		}
	}
}

// TestDSLMutationCoversStep is the ratchet: operator or reasoned skip, for
// every field.
func TestDSLMutationCoversStep(t *testing.T) {
	covered := map[string]bool{}
	for _, operator := range stepOperators {
		if covered[operator.tag] {
			t.Errorf("two operators for %s", operator.tag)
		}

		covered[operator.tag] = true
	}

	for tag := range yamlTagNames(reflect.TypeOf(config.Step{})) {
		if covered[tag] {
			continue
		}

		if reason, skipped := stepMutationSkips[tag]; skipped {
			if reason == "" {
				t.Errorf("stepMutationSkips[%q] has no reason", tag)
			}

			continue
		}

		t.Errorf("config.Step field %q can be neither broken nor explained: add a stepOperator, or a stepMutationSkips entry saying why breaking it would change nothing observable", tag)
	}

	for tag := range stepMutationSkips {
		if covered[tag] {
			t.Errorf("%q is both skipped and operated on", tag)
		}
	}
}

// detectFieldSomewhere breaks the field at every site in the corpus that has
// one, stopping at the first the build notices, and reports how.
//
// Every site rather than only the first, because "the first example that
// happens to use this field" is arbitrary: agents-readonly writes a file with
// a run: whose contents a scripted model never reads back, so a run: mutation
// there is genuinely invisible while the same mutation two pages later is
// caught immediately. The claim worth making is that breaking the field is
// noticed SOMEWHERE, and the site count in the failure message says how hard
// the corpus tried before giving up.
func detectFieldSomewhere(t *testing.T, blocks []docs.Block, operator stepOperator) string {
	t.Helper()

	sites := 0

	for _, block := range blocks {
		for index := 0; ; index++ {
			doc := decodeBlock(t, block)
			if !applyToNthStep(t, doc, operator, index) {
				break
			}

			sites++

			mode := detectMutant(t, block, encodeBlock(t, doc))
			if mode != "" {
				return mode
			}
		}
	}

	if sites == 0 {
		t.Fatalf("no executed doc block has a step with %s: on it — document the field in a runnable example, or add it to stepMutationSkips with a reason", operator.tag)
	}

	t.Fatalf("breaking %s: at all %d site(s) in the corpus changed nothing it checks — the examples using it assert around the field, not on it", operator.tag, sites)

	return ""
}

// applyToNthStep applies the operator to the nth step it can actually mutate,
// counting from zero, and reports whether there was one.
//
// "Can actually mutate" is decided by running the operator against a deep copy
// first, because carrying the field is not the same as being falsifiable: an
// empty list or a one-entry verdict vocabulary declines. Counting positions
// where the field is merely PRESENT would let a declining site absorb an
// index, so index 0 and index 1 would both land on the next site — the same
// mutant run twice, and a site count in the failure message larger than the
// number of sites really tried.
func applyToNthStep(t *testing.T, doc map[string]any, operator stepOperator, n int) bool {
	t.Helper()

	remaining := n
	walker := stepOperator{tag: operator.tag, apply: func(step map[string]any) bool {
		if !operator.apply(deepCopyStep(t, step)) {
			return false // nothing here to break; not a site
		}

		if remaining > 0 {
			remaining--

			return false
		}

		return operator.apply(step)
	}}

	jobs, _ := doc["jobs"].([]any)

	for _, entry := range jobs {
		job, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		plan, _ := job["plan"].([]any)
		if applyToSteps(plan, walker) {
			return true
		}
	}

	return false
}

// applyToSteps is applyToFirstStep's recursion, descending into every
// construct that holds another step.
func applyToSteps(steps []any, operator stepOperator) bool {
	for _, entry := range steps {
		step, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		if _, present := step[operator.tag]; present && operator.apply(step) {
			return true
		}

		if applyToNested(step, operator) {
			return true
		}
	}

	return false
}

// applyToNested descends one step's children, through the same walk
// mutation_test.go uses to find assert sites.
func applyToNested(step map[string]any, operator stepOperator) bool {
	for _, group := range nestedStepGroups(step) {
		if applyToSteps(group.steps, operator) {
			return true
		}
	}

	return false
}

// detectMutant runs a mutated block and reports HOW the corpus caught it, or
// "" when nothing did.
func detectMutant(t *testing.T, block docs.Block, body string) string {
	t.Helper()

	for _, key := range []string{"OPENROUTER_API_KEY", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(key, "test-key-not-used-for-any-call")
	}

	scenario := docScenarios[block.TestID()].tolerateOverrun()

	mutated := block
	mutated.Body = body

	dir := t.TempDir()
	path := writeDocBlock(t, dir, mutated, scenario)

	varFlags := scenarioVarFlags(scenario)
	runFlags := scenarioFlags(scenario)

	if run(append([]string{"validate", "--syntax-only", path}, varFlags...)) != nil {
		return "load"
	}

	err := run(append([]string{"test", path}, runFlags...))

	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), "assert."):
		return "assert"
	default:
		return "crash"
	}
}

// deepCopyStep round-trips a step through YAML, so probing whether an
// operator applies cannot leave a half-mutation behind on the real tree. The
// operators write through nested maps and slices, so a shallow copy would not
// isolate anything.
func deepCopyStep(t *testing.T, step map[string]any) map[string]any {
	t.Helper()

	encoded, err := yaml.Marshal(step)
	if err != nil {
		t.Fatalf("copy step: %v", err)
	}

	var copied map[string]any

	err = yaml.Unmarshal(encoded, &copied)
	if err != nil {
		t.Fatalf("copy step: %v", err)
	}

	return copied
}

// runnableBlocks is the executed corpus, in page order.
func runnableBlocks(t *testing.T) []docs.Block {
	t.Helper()

	var out []docs.Block

	for _, block := range mustBlocks(t) {
		if block.Mode() == "run" {
			out = append(out, block)
		}
	}

	return out
}

// The operators themselves. Each is a closure over the key it breaks, so the
// table above reads as a list of fields rather than a list of functions.

func setString(key, value string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		step[key] = value

		return true
	}
}

func setInt(key string, value int) func(map[string]any) bool {
	return func(step map[string]any) bool {
		if current, ok := step[key].(int); ok && current == value {
			return false // already the mutant; nothing would change
		}

		step[key] = value

		return true
	}
}

func deleteKey(key string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		delete(step, key)

		return true
	}
}

func dropFirstOfList(key string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		list, ok := step[key].([]any)
		if !ok || len(list) == 0 {
			return false
		}

		step[key] = list[1:]

		return true
	}
}

func replaceFirstOfList(key, value string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		list, ok := step[key].([]any)
		if !ok || len(list) == 0 {
			return false
		}

		list[0] = value

		return true
	}
}

// truncateList keeps only a block's first child.
func truncateList(key string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		list, ok := step[key].([]any)
		if !ok || len(list) < 2 {
			return false
		}

		step[key] = list[:1]

		return true
	}
}

// truncateBranches drops the last branch of a concurrent block.
func truncateBranches(key, branchKey string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		block, ok := step[key].(map[string]any)
		if !ok {
			return false
		}

		branches, ok := block[branchKey].([]any)
		if !ok || len(branches) < 2 {
			return false
		}

		block[branchKey] = branches[:len(branches)-1]

		return true
	}
}

// mutateFirstMapValue rewrites one entry of a mapping-valued field: an int
// gets incremented (a marker string would make `head -n MUTANT` fail for the
// wrong reason), anything else becomes the marker.
func mutateFirstMapValue(key string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		mapping, ok := step[key].(map[string]any)
		if !ok || len(mapping) == 0 {
			return false
		}

		names := make([]string, 0, len(mapping))
		for name := range mapping {
			names = append(names, name)
		}

		sort.Strings(names) // deterministic pick across runs

		if number, isInt := mapping[names[0]].(int); isInt {
			mapping[names[0]] = number + 1
		} else {
			mapping[names[0]] = mutantMarker
		}

		return true
	}
}

// mutateVersion turns `every` into the default single-version fetch, and
// repoints a pinned version at a different one.
func mutateVersion(step map[string]any) bool {
	switch version := step["version"].(type) {
	case string:
		if version == "every" {
			step["version"] = "latest"

			return true
		}

		return false
	case map[string]any:
		return mutateFirstMapValue("version")(step)
	default:
		return false
	}
}

// mutatePromptFile breaks both spellings: the load-time path and the
// run-time {artifact, path} mapping.
func mutatePromptFile(step map[string]any) bool {
	switch step["prompt_file"].(type) {
	case string:
		step["prompt_file"] = mutantMarker

		return true
	case map[string]any:
		return mutateFirstMapValue("prompt_file")(step)
	default:
		return false
	}
}

// narrowToolsToReadFile replaces a step's tool selection with read_file. If
// the agent grants it, the tool the example's model actually calls is gone;
// if it does not, selecting an ungranted tool is a load error. Either way the
// build has to say something.
func narrowToolsToReadFile(step map[string]any) bool {
	tools, ok := step["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}

	if len(tools) == 1 && tools[0] == "read_file" {
		step["tools"] = []any{"list_dir"}
	} else {
		step["tools"] = []any{"read_file"}
	}

	return true
}

// mutateRouteTargets repoints every branch of a to: map.
func mutateRouteTargets(key string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		routes, ok := step[key].(map[string]any)
		if !ok || len(routes) == 0 {
			return false
		}

		for outcome := range routes {
			routes[outcome] = mutantMarker
		}

		return true
	}
}

// mutateVerdictEntries breaks the first declared verdict, in whichever
// spelling it uses: a bare name is renamed (so the name the model chooses is
// no longer in the vocabulary), a name: target pair is repointed.
func mutateVerdictEntries(step map[string]any) bool {
	entries, ok := step["verdicts"].([]any)
	if !ok || len(entries) == 0 {
		return false
	}

	switch entry := entries[0].(type) {
	case string:
		entries[0] = mutantMarker

		return true
	case map[string]any:
		for name := range entry {
			entry[name] = mutantMarker

			return true
		}

		return false
	default:
		return false
	}
}

// scaleBudgetTokens raises a ceiling far enough that it stops binding.
func scaleBudgetTokens(step map[string]any) bool {
	budget, ok := step["budget"].(map[string]any)
	if !ok {
		return false
	}

	tokens, ok := budget["tokens"].(int)
	if !ok {
		return false
	}

	budget["tokens"] = tokens * 1000

	return true
}

// truncateAcross narrows a matrix to one cell per axis.
func truncateAcross(step map[string]any) bool {
	axes, ok := step["across"].([]any)
	if !ok || len(axes) == 0 {
		return false
	}

	changed := false

	for _, entry := range axes {
		axis, isMap := entry.(map[string]any)
		if !isMap {
			continue
		}

		values, hasValues := axis["values"].([]any)
		if !hasValues || len(values) < 2 {
			continue
		}

		axis["values"] = values[:1]
		changed = true
	}

	return changed
}

// unwrapTry replaces the wrapper with the step it wraps, so the failure it
// was hiding stops the plan.
func unwrapTry(step map[string]any) bool {
	wrapped, ok := step["try"].(map[string]any)
	if !ok {
		return false
	}

	delete(step, "try")

	for key, value := range wrapped {
		step[key] = value
	}

	return true
}
