package merkle

// Hashing a matrix whose width is not knowable at plan time (`from_file:`).

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func fileAxisStep(from, run string) config.Step {
	return config.Step{
		Across: []config.AcrossVar{{Var: "item", FromFile: from}},
		Task:   "work",
		Run:    run,
		Inputs: config.Inputs(),
	}
}

// TestAcrossPlanContentHashesTheDeclaration proves a from_file: matrix hashes
// what IS knowable at plan time — the axis (including its source path) and the
// unexpanded template — rather than cells that do not exist yet.
func TestAcrossPlanContentHashesTheDeclaration(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	content, err := AcrossPlanContent(cfg, fileAxisStep("findings/items.json", "echo {{ .vars.item }}"), 0)
	if err != nil {
		t.Fatalf("AcrossPlanContent: %v", err)
	}

	if _, expanded := content["across"]; expanded {
		t.Error("a from_file: matrix hashed expanded cells; its width is not knowable at plan time")
	}

	if _, declared := content["across_runtime"]; !declared {
		t.Fatalf("content = %#v, want the across_runtime marker", content)
	}
}

// TestAcrossPlanContentBustsOnTheSourcePath proves the source is part of the
// block's identity: pointing an axis at a different file is a different block,
// even though neither file has been read.
func TestAcrossPlanContentBustsOnTheSourcePath(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	hash := func(from, run string) string {
		t.Helper()

		content, err := AcrossPlanContent(cfg, fileAxisStep(from, run), 0)
		if err != nil {
			t.Fatalf("AcrossPlanContent: %v", err)
		}

		out, err := HashNode(NodeKindAcross, content, "parent")
		if err != nil {
			t.Fatalf("HashNode: %v", err)
		}

		return out
	}

	base := hash("findings/items.json", "echo {{ .vars.item }}")

	if base == hash("findings/others.json", "echo {{ .vars.item }}") {
		t.Error("changing from_file: must change the block's hash")
	}

	if base == hash("findings/items.json", "echo other {{ .vars.item }}") {
		t.Error("changing the cell template must change the block's hash")
	}
}

// TestAcrossFileMatrixDoesNotCollideWithAStaticOne is what the marker exists
// for: a file matrix and a static one that happens to render the same way are
// different blocks, because only one of them can be predicted at plan time.
func TestAcrossFileMatrixDoesNotCollideWithAStaticOne(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	fileContent, err := AcrossPlanContent(cfg, fileAxisStep("findings/items.json", "echo hi"), 0)
	if err != nil {
		t.Fatalf("AcrossPlanContent(file): %v", err)
	}

	staticStep := config.Step{
		Across: []config.AcrossVar{{Var: "item", Values: []string{"a"}}},
		Task:   "work",
		Run:    "echo hi",
		Inputs: config.Inputs(),
	}

	staticContent, err := AcrossPlanContent(cfg, staticStep, 0)
	if err != nil {
		t.Fatalf("AcrossPlanContent(static): %v", err)
	}

	fileHash, err := HashNode(NodeKindAcross, fileContent, "parent")
	if err != nil {
		t.Fatalf("HashNode: %v", err)
	}

	staticHash, err := HashNode(NodeKindAcross, staticContent, "parent")
	if err != nil {
		t.Fatalf("HashNode: %v", err)
	}

	if fileHash == staticHash {
		t.Error("a from_file: matrix and a static one hashed alike; the planner can predict only one of them")
	}
}

// TestCellHashTaskWithOutputsIsNotCacheable closes the cached-cell hole: a
// skipped cell captures nothing, so a rerun would hand the consumer of the
// collected artifact a directory this cell's contribution is missing from —
// and the store keeps no artifact bytes to replay. Both spellings of the
// declaration count, since executeTask captures the RESOLVED outputs.
func TestCellHashTaskWithOutputsIsNotCacheable(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Tasks: []config.Task{{Name: "declared", Run: "true", Outputs: []string{"findings"}}}}

	cacheable := func(cell config.Step) bool {
		t.Helper()

		_, ok, err := CellHash(cfg, cell, "parent")
		if err != nil {
			t.Fatalf("CellHash: %v", err)
		}

		return ok
	}

	if !cacheable(config.Step{Task: "work", Run: "true", Inputs: config.Inputs()}) {
		t.Error("a plain task cell must stay cacheable")
	}

	if cacheable(config.Step{Task: "work", Run: "true", Inputs: config.Inputs(), Outputs: []string{"findings"}}) {
		t.Error("a task cell declaring outputs must not be cacheable; a skipped cell captures nothing")
	}

	if cacheable(config.Step{Task: "declared", Inputs: config.Inputs()}) {
		t.Error("outputs inherited from the tasks: entry must count too; capture uses the resolved set")
	}
}

// TestAcrossFileMatrixForcesItsProducerToRerun is the load-bearing cache
// property, and the reason this feature needs no store-style replay.
//
// The producing task and the matrix sit on ONE chain, and a chain containing an
// across: block is unskippable — so the producer is forced to re-run every run
// and the file the axis reads is always the file this run wrote. A cached
// producer would leave the matrix expanding over a stale (or absent) list.
func TestAcrossFileMatrixForcesItsProducerToRerun(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	steps := []config.Step{
		{Task: "scan", Run: "true", Inputs: config.Inputs(), Outputs: []string{"findings"}},
		fileAxisStep("findings/items.json", "echo {{ .vars.item }}"),
	}

	chains, err := PlanChains(context.Background(), cfg, "build", steps, nil, nil, nil)
	if err != nil {
		t.Fatalf("PlanChains: %v", err)
	}

	if len(chains) != 1 {
		t.Fatalf("len(chains) = %d, want 1", len(chains))
	}

	if !chains[0].Unskippable {
		t.Error("the chain is skippable; a cached producer would leave the matrix reading a stale list")
	}
}
