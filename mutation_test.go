package main

// Mutation testing for the doc corpus assertions.
//
// docs_test.go proves every example RUNS and that every job carries an
// assert:. Neither proves an assertion can FAIL. An assert.execution that
// happens to match, a stdout: substring that appears in the prose anyway, a
// files: entry naming something another step wrote — each passes forever
// while checking nothing, and each looks exactly like a real assertion on the
// page.
//
// So: take each assertion in turn, rewrite it to something the pipeline does
// not satisfy, and require the build to go red. Three things are checked per
// mutant, in order, and the order is the point:
//
//  1. `steps validate --syntax-only` must still PASS — the mutant has to be a
//     legal pipeline whose only defect is the expectation. Without this a
//     mutation that makes the file fail to LOAD would count as "caught",
//     proving nothing about the assertion. assert.verdict is the live case:
//     naming an undeclared verdict is a load error, so the operator swaps in
//     another DECLARED one instead.
//  2. `steps test` must FAIL.
//  3. the error must name an `assert.` field — the difference between "the
//     assertion caught it" and "the pipeline broke some other way".
//
// The mutants are built by rewriting the block's decoded YAML tree rather
// than its config.Config: a Config round-trip loses omitempty shape, resolved
// includes, registered builtins and applied defaults, so the pipeline that
// ran would not be the pipeline the page shows. The generic tree is what
// injectFakeProvider already rewrites for the same reason.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/docs"
	"github.com/jtarchie/steps/internal/pipeline"
)

// mutantMarker is the text an operator splices in where a match must fail.
// Distinctive so a mutant that somehow passes is greppable in the output.
const mutantMarker = "MUTANT-NO-SUCH-THING"

// mutant is one rewritten copy of a block: the YAML to run, and the label
// naming which assertion was broken to produce it.
type mutant struct {
	label string
	body  string
}

// TestAssertMutation is the whole suite: every assertion in every executed
// doc block, broken one at a time.
func TestAssertMutation(t *testing.T) {
	total := 0

	for _, block := range mustBlocks(t) {
		if block.Mode() != "run" {
			continue
		}

		mutants := blockMutants(t, block)
		if len(mutants) == 0 {
			t.Errorf("%s: no assertion could be mutated — an executed block asserts nothing falsifiable", block.Name())

			continue
		}

		total += len(mutants)

		for _, mutant := range mutants {
			t.Run(block.Name()+"/"+mutant.label, func(t *testing.T) {
				checkMutantIsCaught(t, block, mutant)
			})
		}
	}

	t.Logf("%d mutants across the doc corpus", total)
}

// checkMutantIsCaught runs one mutant through the three checks above.
func checkMutantIsCaught(t *testing.T, block docs.Block, mut mutant) {
	t.Helper()

	// Same reason runDocBlock resets it (see its own comment): a block whose
	// fallback: fires pins that agent name process-wide, and the pinned source
	// carries the endpoint of a fake server this mutant is about to tear down.
	// Every mutant re-runs the same block in this one process, so without the
	// reset the SECOND mutant of a failing-over example runs against the
	// FIRST's dead endpoint — failing for a reason that has nothing to do with
	// the assertion under test, which is exactly what a mutant surviving
	// looks like.
	pipeline.ResetPreflightCache()
	t.Cleanup(pipeline.ResetPreflightCache)

	for _, key := range []string{"OPENROUTER_API_KEY", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(key, "test-key-not-used-for-any-call")
	}

	scenario := docScenarios[block.TestID()].tolerateOverrun()

	mutated := block
	mutated.Body = mut.body

	dir := t.TempDir()
	path := writeDocBlock(t, dir, mutated, scenario)

	err := run(append([]string{"validate", "--syntax-only", path}, scenarioVarFlags(scenario)...))
	if err != nil {
		t.Fatalf("the mutant does not load, so a failing run would prove nothing about the assertion: %v", err)
	}

	// scenarioFlags, not just the vars: a mutant has to be invoked exactly as
	// the doc harness invokes the original. Missing a flag here makes the run
	// fail for the wrong reason — an unmapped worker rather than the broken
	// assertion — and the mutant reads as uncaught.
	err = run(append([]string{"test", path}, scenarioFlags(scenario)...))
	if err == nil {
		t.Fatalf("the mutated assertion still passed — as written it cannot fail, so it verifies nothing")
	}

	if !strings.Contains(err.Error(), "assert.") {
		t.Fatalf("the run failed, but not on an assertion — something else broke:\n%v", err)
	}
}

// blockMutants builds one mutant per mutable assertion field in a block.
func blockMutants(t *testing.T, block docs.Block) []mutant {
	t.Helper()

	var doc map[string]any

	err := yaml.Unmarshal([]byte(block.Body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	var mutants []mutant

	// Each site is re-decoded from the original text so operators never see
	// another operator's edit — one mutant, one broken assertion.
	for _, site := range assertSites(doc) {
		for _, field := range site.fields() {
			fresh := decodeBlock(t, block)

			target := resolvePath(fresh, site.path)
			if target == nil {
				t.Fatalf("%s: assert site %v vanished on re-decode", block.Name(), site.path)
			}

			if !mutateField(target, site.step, field) {
				continue
			}

			mutants = append(mutants, mutant{
				label: strings.Join(site.path, ".") + "." + field,
				body:  encodeBlock(t, fresh),
			})
		}
	}

	return mutants
}

// assertSite is one assert: mapping found in a block, with the path to reach
// it again in a freshly decoded copy and (for a step assert) the step that
// owns it — assert.verdict can only be mutated against the step's own
// declared verdicts:.
type assertSite struct {
	path   []string
	assert map[string]any
	step   map[string]any
}

// fields names the assert keys this site can have mutated, in a stable order.
func (s assertSite) fields() []string {
	names := make([]string, 0, len(s.assert))
	for name := range s.assert {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// assertSites finds every assert: mapping in a decoded block — pipeline,
// job, and step, including steps nested in do:/in_parallel:/race:/ensemble:
// blocks, a try: wrapper, and hooks.
func assertSites(doc map[string]any) []assertSite {
	var sites []assertSite

	if assert, ok := doc["assert"].(map[string]any); ok {
		sites = append(sites, assertSite{path: []string{"assert"}, assert: assert})
	}

	jobs, _ := doc["jobs"].([]any)
	for i, entry := range jobs {
		job, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		base := []string{"jobs", strconv.Itoa(i)}

		if assert, found := job["assert"].(map[string]any); found {
			sites = append(sites, assertSite{path: append(base, "assert"), assert: assert})
		}

		plan, _ := job["plan"].([]any)
		sites = append(sites, stepSites(append(base, "plan"), plan)...)
	}

	return sites
}

// stepSites walks a list of steps collecting their assert sites, descending
// into every construct that can hold another step.
func stepSites(base []string, steps []any) []assertSite {
	var sites []assertSite

	for i, entry := range steps {
		step, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		path := append(append([]string{}, base...), strconv.Itoa(i))

		if assert, found := step["assert"].(map[string]any); found {
			sites = append(sites, assertSite{
				path:   append(append([]string{}, path...), "assert"),
				assert: assert,
				step:   step,
			})
		}

		sites = append(sites, nestedStepSites(path, step)...)
	}

	return sites
}

// nestedStepSites descends one step's children.
func nestedStepSites(path []string, step map[string]any) []assertSite {
	groups := nestedStepGroups(step)
	sites := make([]assertSite, 0, len(groups))

	for _, group := range groups {
		sites = append(sites, stepSites(append(append([]string{}, path...), group.keys...), group.steps)...)
	}

	return sites
}

// nestedGroup is one list of steps nested inside another step, with the path
// segments that reach it.
type nestedGroup struct {
	keys  []string
	steps []any
}

// stepContainerKeys are the block fields holding concurrent branches, paired
// with the key their branch list sits under.
var stepContainerKeys = map[string][]string{ //nolint:gochecknoglobals // a test's data table
	"in_parallel": {"steps"},
	"race":        {"steps"},
	"ensemble":    {"agents"},
}

// hookKeys are the five hook fields, each holding a single step.
var hookKeys = []string{"on_success", "on_failure", "on_error", "on_abort", "ensure"} //nolint:gochecknoglobals // a test's data table

// nestedStepGroups returns every list of steps nested inside one step: what a
// try: wraps, a do: block's children, a concurrent block's branches, and each
// hook. Shared by both mutation harnesses so neither can silently stop
// descending into a construct the other still walks.
func nestedStepGroups(step map[string]any) []nestedGroup {
	var groups []nestedGroup

	single := func(key string) {
		if nested, ok := step[key].(map[string]any); ok {
			groups = append(groups, nestedGroup{keys: []string{key}, steps: []any{nested}})
		}
	}

	single("try")

	if nested, ok := step["do"].([]any); ok {
		groups = append(groups, nestedGroup{keys: []string{"do"}, steps: nested})
	}

	for key, branchKeys := range stepContainerKeys {
		block, ok := step[key].(map[string]any)
		if !ok {
			continue
		}

		for _, branchKey := range branchKeys {
			if branches, found := block[branchKey].([]any); found {
				groups = append(groups, nestedGroup{keys: []string{key, branchKey}, steps: branches})
			}
		}
	}

	for _, hook := range hookKeys {
		single(hook)
	}

	return groups
}

// mutateField rewrites one assert field so it can no longer match, reporting
// whether it managed to. A field with nothing falsifiable in it — an empty
// list, a verdict vocabulary with only one name — reports false and is
// skipped rather than producing a mutant that would pass for the wrong
// reason.
//
//nolint:cyclop // one branch per assert field is the clearest shape this can take
func mutateField(assert map[string]any, step map[string]any, field string) bool {
	switch field {
	case "execution":
		return mutateFirstOfList(assert, field)

	case "outcome":
		// The two legal values, swapped. Anything else is a load error, and
		// a load error is exactly what check 1 refuses to accept as a catch.
		if assert[field] == "failed" {
			assert[field] = "succeeded"
		} else {
			assert[field] = "failed"
		}

		return true

	case "stdout":
		assert[field] = mutantMarker + fmt.Sprint(assert[field])

		return true

	case "code":
		code, ok := assert[field].(int)
		if !ok {
			return false
		}

		assert[field] = code + 1

		return true

	case "verdict":
		return mutateVerdict(assert, step)

	case "files":
		// Same declared output, a file inside it that nothing wrote — the
		// first path segment must keep naming a declared output: or the
		// mutant would not load.
		return mutateLastOfList(assert, field, func(path string) string {
			return path + "." + mutantMarker
		})

	case "tool_calls":
		return mutateToolCalls(assert)

	default:
		return false
	}
}

// mutateFirstOfList replaces a list's first entry with the marker.
func mutateFirstOfList(assert map[string]any, field string) bool {
	list, ok := assert[field].([]any)
	if !ok || len(list) == 0 {
		return false
	}

	list[0] = mutantMarker

	return true
}

// mutateLastOfList rewrites a list's last entry through fn.
func mutateLastOfList(assert map[string]any, field string, fn func(string) string) bool {
	list, ok := assert[field].([]any)
	if !ok || len(list) == 0 {
		return false
	}

	last, ok := list[len(list)-1].(string)
	if !ok {
		return false
	}

	list[len(list)-1] = fn(last)

	return true
}

// mutateVerdict swaps the asserted verdict for a DIFFERENT declared one.
// Naming an undeclared verdict is a load error, so it would fail check 1
// rather than proving the assertion binds; a step declaring exactly one
// verdict has no other name to swap in and is skipped.
func mutateVerdict(assert map[string]any, step map[string]any) bool {
	want, ok := assert["verdict"].(string)
	if !ok {
		return false
	}

	for _, name := range declaredVerdicts(step) {
		if name != want {
			assert["verdict"] = name

			return true
		}
	}

	return false
}

// declaredVerdicts reads a step's verdicts: vocabulary in both spellings: a
// bare name, or a single-key {name: target} pair. The reserved failure:
// entry is excluded — the model can never choose it, so asserting on it
// could not be satisfied by any run.
func declaredVerdicts(step map[string]any) []string {
	entries, _ := step["verdicts"].([]any)

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		switch typed := entry.(type) {
		case string:
			names = append(names, typed)
		case map[string]any:
			for name := range typed {
				names = append(names, name)
			}
		}
	}

	return slicesWithout(names, "failure")
}

// slicesWithout returns names with drop removed.
func slicesWithout(names []string, drop string) []string {
	out := names[:0]

	for _, name := range names {
		if name != drop {
			out = append(out, name)
		}
	}

	return out
}

// mutateToolCalls renames the first expected call to a tool that does not
// exist. Renaming rather than touching args: because an args mutation on a
// PINNED key is a load error, and which keys are pinned lives on the agent
// rather than on the step this operator can see.
func mutateToolCalls(assert map[string]any) bool {
	calls, ok := assert["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return false
	}

	first, ok := calls[0].(map[string]any)
	if !ok {
		return false
	}

	first["name"] = mutantMarker

	return true
}

// resolvePath walks a decoded tree by the path an assertSite recorded.
func resolvePath(doc map[string]any, path []string) map[string]any {
	var current any = doc

	for _, segment := range path {
		switch node := current.(type) {
		case map[string]any:
			current = node[segment]
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index >= len(node) {
				return nil
			}

			current = node[index]
		default:
			return nil
		}
	}

	resolved, _ := current.(map[string]any)

	return resolved
}

// decodeBlock decodes a block's body into a fresh tree.
func decodeBlock(t *testing.T, block docs.Block) map[string]any {
	t.Helper()

	var doc map[string]any

	err := yaml.Unmarshal([]byte(block.Body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	return doc
}

// encodeBlock re-marshals a mutated tree back to YAML.
func encodeBlock(t *testing.T, doc map[string]any) string {
	t.Helper()

	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal mutated block: %v", err)
	}

	return string(body)
}
