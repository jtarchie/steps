package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/docs"
	"github.com/jtarchie/steps/internal/config"
)

// The docs ARE the tests: every fenced ```yaml block in docs/*.md is
// extracted here and — per its fence-info mode (see package docs) —
// schema-validated, loaded, and executed. A doc example that stops working
// fails the build, which is the entire sync mechanism; there is no separate
// example corpus to keep honest.
//
// Agent examples run against the same fake provider the e2e tests use
// (fakeprovider_test.go). The rendered doc stays bare-minimum: the fence
// carries test=<id>, and docScenarios (docs_scenarios_test.go) supplies the
// scripted turns, any files the pipeline expects on disk, and Go-side
// assertions. The harness rewrites each agent's source: to point at the
// fake endpoint before running — the same source.endpoint: seam, applied to
// YAML the reader never sees mutated.

// mustBlocks extracts every yaml block from the embedded docs, failing on
// authoring errors (an unterminated fence).
func mustBlocks(t *testing.T) []docs.Block {
	t.Helper()

	blocks, err := docs.Blocks()
	if err != nil {
		t.Fatal(err)
	}

	if len(blocks) == 0 {
		t.Fatal("no yaml blocks found in docs/*.md")
	}

	return blocks
}

// yamlStringAsJSONValue converts YAML source to the any-typed value the
// schema validator expects — the string twin of yamlAsJSONValue
// (schema_test.go), for blocks that never exist as files.
func yamlStringAsJSONValue(t *testing.T, source string) any {
	t.Helper()

	path := filepath.Join(t.TempDir(), "block.yml")

	err := os.WriteFile(path, []byte(source), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return yamlAsJSONValue(t, path)
}

// TestDocsExamples is the extracted-block pipeline: schema → load/validate →
// run. Fragments are rendered prose and skipped; noexec blocks stop after
// validation (they need docker, the network, or real credentials); run
// blocks execute end to end via the same run() the CLI uses.
func TestDocsExamples(t *testing.T) {
	schema := loadSchema(t)

	for _, block := range mustBlocks(t) {
		t.Run(block.Name(), func(t *testing.T) {
			runDocBlock(t, schema, block)
		})
	}
}

// runDocBlock is one block's whole treatment: schema validation, syntax-only
// validate, and — for run-mode blocks — full validate of the original text
// plus end-to-end execution and the scenario's own assertions.
func runDocBlock(t *testing.T, schema *jsonschema.Schema, block docs.Block) {
	t.Helper()

	if block.Mode() == "fragment" {
		t.Skip("fragment: rendered only")
	}

	err := schema.Validate(yamlStringAsJSONValue(t, block.Body))
	if err != nil {
		t.Fatalf("does not match steps.schema.json:\n%v", err)
	}

	scenario, hasScenario := docScenarios[block.TestID()]
	if block.TestID() != "" && !hasScenario {
		t.Fatalf("fence names test=%s but docs_scenarios_test.go has no such scenario", block.TestID())
	}

	dir := t.TempDir()
	path := writeDocBlock(t, dir, block, scenario)

	varFlags := make([]string, 0, 2*len(scenario.vars))
	for name, value := range scenario.vars {
		varFlags = append(varFlags, "--var", name+"="+value)
	}

	err = run(append([]string{"validate", "--syntax-only", path}, varFlags...))
	if err != nil {
		t.Fatalf("steps validate: %v", err)
	}

	if block.Mode() == "noexec" {
		return
	}

	executeDocBlock(t, block, scenario, dir, path, varFlags)
}

// executeDocBlock is the run-mode half: full validate of the block's ORIGINAL
// text (not the fake-injected rewrite — the credential/provider checks
// --syntax-only skips are exactly what a reader's copy hits first), then
// end-to-end execution, then the scenario's own assertions. One dummy key per
// provider the docs reach for, so a block naming a provider with no key here
// fails loudly instead of silently skipping the check. noexec blocks never
// get here: their stdio MCP binaries aren't on this host.
func executeDocBlock(t *testing.T, block docs.Block, scenario docScenario, dir, path string, varFlags []string) {
	t.Helper()

	for _, key := range []string{"OPENROUTER_API_KEY", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(key, "test-key-not-used-for-any-call")
	}

	// Written beside the injected pipeline so the scenario's files
	// (run_file:/file: targets) resolve for it too.
	original := filepath.Join(dir, "original.yml")

	err := os.WriteFile(original, []byte(block.Body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = run(append([]string{"validate", original}, varFlags...))
	if err != nil {
		t.Fatalf("steps validate (full, original text): %v", err)
	}

	err = run(append([]string{"test", path}, varFlags...))
	if err != nil {
		t.Fatalf("steps test: %v", err)
	}

	if scenario.check != nil {
		scenario.check(t, dir)
	}
}

// writeDocBlock materializes a block into dir: any files the scenario
// declares, plus the pipeline itself — with every agent pointed at the fake
// provider when the scenario scripts one.
func writeDocBlock(t *testing.T, dir string, block docs.Block, scenario docScenario) string {
	t.Helper()

	for name, body := range scenario.files {
		full := filepath.Join(dir, name)

		err := os.MkdirAll(filepath.Dir(full), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(full, []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	body := block.Body

	if usesAgents(t, body) {
		if block.Mode() == "run" && scenario.fake == nil {
			t.Fatalf("%s runs agent steps and must carry test=<id> naming a scenario with a scripted provider", block.Name())
		}

		if scenario.fake != nil {
			body = injectFakeProvider(t, body, scenario.fake(t).URL)
			t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")
		}
	}

	pipelinePath := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(pipelinePath, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return pipelinePath
}

// usesAgents reports whether the pipeline defines agents: — the signal that
// running it will reach a provider.
func usesAgents(t *testing.T, body string) bool {
	t.Helper()

	var doc map[string]any

	err := yaml.Unmarshal([]byte(body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	agents, ok := doc["agents"].([]any)

	return ok && len(agents) > 0
}

// injectFakeProvider rewrites every agent's source: to the fake endpoint and
// disables preflight, leaving everything else the doc showed intact. The
// reader sees a real model name; the test sees a scripted one — the same
// substitution a $STEPS_MODEL override performs, done structurally.
func injectFakeProvider(t *testing.T, body, endpoint string) string {
	t.Helper()

	var doc map[string]any

	err := yaml.Unmarshal([]byte(body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	agents, _ := doc["agents"].([]any)
	for _, entry := range agents {
		agent, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("agents: entry is not a mapping: %v", entry)
		}

		agent["source"] = map[string]any{
			"endpoint":    endpoint + "/v1/",
			"model":       "test-model",
			"api_key_env": "STEPS_TEST_AGENT_API_KEY",
		}
	}

	defaults, ok := doc["defaults"].(map[string]any)
	if !ok {
		defaults = map[string]any{}
		doc["defaults"] = defaults
	}

	// A defaults.model would fight the per-agent source above; preflight
	// would spend a scripted turn on a probe (see fakeprovider_test.go).
	delete(defaults, "model")
	defaults["preflight"] = map[string]any{"disabled": true}

	rewritten, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal block: %v", err)
	}

	return string(rewritten)
}

// TestDocsExamplesAssert is the ratchet that keeps the corpus from decaying
// back into smoke tests: every job of every executed block must carry a
// job-level assert: with BOTH execution: and outcome:, and a block declaring
// more than one job must also carry the pipeline-level assert.execution that
// names them.
//
// Without it a doc example proves only "this loads and exits 0" — 58 of the
// 63 executed blocks were in exactly that state when this was written, so
// `upper` could stop uppercasing and every one of them stayed green. Both
// fields are required because each covers the other's blind spot:
// execution: names which steps ran in what order (the load-bearing half, and
// the only way to prove something did NOT run), while outcome: is what stops
// a matching execution: from CLEARING a real plan failure — the clearing rule
// makes execution: alone unable to tell a fixed build from a broken one.
//
// Job-level rather than step-level because it is the one position always
// available: assert: is rejected on get/put steps and anywhere inside a try:,
// and on a task with a fix: it would take over the success decision the fix:
// exists to make. Step asserts are the richer check where they are legal, and
// examples carry them too — they just cannot be the rule.
func TestDocsExamplesAssert(t *testing.T) {
	for _, block := range mustBlocks(t) {
		if block.Mode() != "run" {
			continue // fragments are prose; noexec blocks never execute
		}

		t.Run(block.Name(), func(t *testing.T) {
			checkBlockAsserts(t, block)
		})
	}
}

// checkBlockAsserts enforces the rule above for one block.
func checkBlockAsserts(t *testing.T, block docs.Block) {
	t.Helper()

	var doc struct {
		Assert *config.Assert `yaml:"assert"`
		Jobs   []struct {
			Name   string         `yaml:"name"`
			Assert *config.Assert `yaml:"assert"`
		} `yaml:"jobs"`
	}

	err := yaml.Unmarshal([]byte(block.Body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	if len(doc.Jobs) == 0 {
		t.Fatal("an executed block must declare at least one job")
	}

	for _, job := range doc.Jobs {
		switch {
		case job.Assert == nil:
			t.Errorf("job %q has no assert: — an executed example must verify itself, not just run", job.Name)
		case len(job.Assert.Execution) == 0:
			t.Errorf("job %q asserts no execution: — name the steps that must run, in order", job.Name)
		case job.Assert.Outcome == "":
			t.Errorf("job %q asserts no outcome: — a matching execution: CLEARS a plan failure, so without it a broken build passes too", job.Name)
		}
	}

	if len(doc.Jobs) > 1 && (doc.Assert == nil || len(doc.Assert.Execution) == 0) {
		t.Errorf("a block with %d jobs needs a pipeline-level assert.execution naming them in order", len(doc.Jobs))
	}
}

// TestDocsNoexecReasons makes opting out of execution cost a sentence. A
// noexec block is validated but never run, so it is the one kind that can rot
// silently — `noexec` alone was the cheapest way to make a stubborn example
// stop failing. The reason must come from docs.NoexecReasons(), so "which
// examples would run if this host had docker?" stays a grep.
func TestDocsNoexecReasons(t *testing.T) {
	allowed := map[string]bool{}
	for _, reason := range docs.NoexecReasons() {
		allowed[reason] = true
	}

	for _, block := range mustBlocks(t) {
		if block.Mode() != "noexec" {
			continue
		}

		reason := block.NoexecReason()

		switch {
		case reason == "":
			t.Errorf("%s: bare `noexec` — say which capability this host lacks (noexec=<%s>)",
				block.Name(), strings.Join(docs.NoexecReasons(), "|"))
		case !allowed[reason]:
			t.Errorf("%s: noexec=%s is not one of %v — add it to docs.NoexecReasons if it is a real new class",
				block.Name(), reason, docs.NoexecReasons())
		}
	}
}

// TestDocsCoverage is the schema_test pattern applied to prose: every yaml
// key on config.Step must appear in at least one *tested* doc block. Adding
// a DSL field without documenting it is a red build, not a forgotten TODO.
func TestDocsCoverage(t *testing.T) {
	used := map[string]bool{}

	for _, block := range mustBlocks(t) {
		if block.Mode() == "fragment" {
			continue // fragments are unvalidated prose; they don't count
		}

		var doc any

		err := yaml.Unmarshal([]byte(block.Body), &doc)
		if err != nil {
			t.Fatalf("%s: %v", block.Name(), err)
		}

		collectKeys(doc, used)
	}

	var missing []string

	for tag := range yamlTagNames(reflect.TypeOf(config.Step{})) {
		if !used[tag] {
			missing = append(missing, tag)
		}
	}

	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("config.Step fields with no tested doc example (add one to docs/*.md): %v", missing)
	}
}

// collectKeys walks a decoded YAML value recording every mapping key.
func collectKeys(value any, keys map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			keys[key] = true

			collectKeys(child, keys)
		}
	case []any:
		for _, child := range typed {
			collectKeys(child, keys)
		}
	}
}

// docLinkPattern matches [text](target) inline links. Good enough for this
// corpus: the docs never nest brackets inside link text.
var docLinkPattern = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestDocsAnchors is link integrity for the corpus: every relative .md link
// must name an existing page, and every #fragment (same-page or cross-page)
// must name a heading id that page actually renders. The ids come from
// docs.Headings — the same GitHub-style slugs the web renderer emits — so a
// broken anchor is a red build here instead of a silent jump-to-top in the
// UI. (The YAML examples got this guarantee first; the anchors were the
// untested half.)
func TestDocsAnchors(t *testing.T) {
	ids := map[string]map[string]bool{}

	for _, page := range docs.Pages() {
		body, err := docs.Page(page)
		if err != nil {
			t.Fatal(err)
		}

		ids[page] = map[string]bool{}
		for _, heading := range docs.Headings(string(body)) {
			ids[page][heading.ID] = true
		}
	}

	for _, page := range docs.Pages() {
		body, _ := docs.Page(page)

		for _, match := range docLinkPattern.FindAllStringSubmatch(string(body), -1) {
			checkDocLink(t, ids, page, match[1])
		}
	}
}

// checkDocLink verifies one link target against the corpus's rendered ids.
func checkDocLink(t *testing.T, ids map[string]map[string]bool, page, target string) {
	t.Helper()

	if strings.Contains(target, "://") || strings.HasPrefix(target, "..") {
		return // external, or repo files outside the embed
	}

	targetPage, fragment, _ := strings.Cut(target, "#")
	if targetPage == "" {
		targetPage = page // same-page #fragment
	}

	pageIDs, exists := ids[targetPage]
	if !exists {
		t.Errorf("%s links to %q, which is not an embedded doc page", page, target)

		return
	}

	if fragment != "" && !pageIDs[fragment] {
		t.Errorf("%s links to %q, but %s has no heading with id %q", page, target, targetPage, fragment)
	}
}

// TestDocsGroupsComplete keeps the curated navigation table (docs.Groups)
// covering every page: a new page must be placed in a group, or the web
// rail would silently omit it.
func TestDocsGroupsComplete(t *testing.T) {
	grouped := map[string]bool{}

	for _, group := range docs.Groups() {
		for _, page := range group.Pages {
			if grouped[page] {
				t.Errorf("docs.Groups lists %s twice", page)
			}

			grouped[page] = true
		}
	}

	for _, page := range docs.Pages() {
		if page == "README.md" {
			continue // the index is the rail's "start here", not a group member
		}

		if !grouped[page] {
			t.Errorf("docs.Groups has no group for %s — add it to the table in docs/docs.go", page)
		}
	}

	for page := range grouped {
		_, err := docs.Page(page)
		if err != nil {
			t.Errorf("docs.Groups names %s, which is not an embedded page", page)
		}
	}
}

// TestDocsPagesListed keeps the index honest: every page docs.Pages reports
// must be linked from README.md, so nothing ships unfindable.
func TestDocsPagesListed(t *testing.T) {
	index, err := docs.Page("README.md")
	if err != nil {
		t.Fatal(err)
	}

	for _, page := range docs.Pages() {
		if page == "README.md" {
			continue
		}

		if !strings.Contains(string(index), fmt.Sprintf("(%s)", page)) {
			t.Errorf("docs/README.md has no link to %s", page)
		}
	}
}
