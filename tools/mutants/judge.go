package main

// judge: which e2e test notices which change. coverattr says what only e2e executes; executing is not asserting, so each conditional in those blocks is negated, the e2e binary rebuilt with the change overlaid, and only the tests that execute it are run. A test that is the only one to fail for some mutant is the only one guarding that behaviour; one that never is can go without losing anything a mutant can see.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// negations are gremlins' CONDITIONALS_NEGATION and INVERT_LOGICAL: each flips the answer and still type-checks.
var negations = map[token.Token]token.Token{ //nolint:gochecknoglobals // a table
	token.EQL: token.NEQ, token.NEQ: token.EQL,
	token.LSS: token.GEQ, token.GEQ: token.LSS,
	token.GTR: token.LEQ, token.LEQ: token.GTR,
	token.LAND: token.LOR, token.LOR: token.LAND,
}

type attributedBlock struct {
	Block string   `json:"block"`
	Tests []string `json:"tests"`
}

type attributedTest struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
}

type mutant struct {
	File   string   `json:"file"`
	Line   int      `json:"line"`
	Column int      `json:"column"`
	From   string   `json:"from"`
	To     string   `json:"to"`
	Tests  []string `json:"tests"`
	offset int

	Outcome string   `json:"outcome"`
	Killers []string `json:"killers,omitempty"`
}

// span is a cover block's extent: "file:startLine.startCol,endLine.endCol".
type span struct {
	file                                 string
	startLine, startCol, endLine, endCol int
}

func parseSpan(block string) (span, error) {
	file, rest, ok := strings.Cut(block, ":")
	if !ok {
		return span{}, fmt.Errorf("block %q has no position", block)
	}

	var s span

	_, err := fmt.Sscanf(rest, "%d.%d,%d.%d", &s.startLine, &s.startCol, &s.endLine, &s.endCol)
	if err != nil {
		return span{}, fmt.Errorf("block %q: %w", block, err)
	}

	s.file = strings.TrimPrefix(file, "github.com/jtarchie/steps/")

	return s, nil
}

func (s span) contains(p token.Position) bool {
	after := p.Line > s.startLine || (p.Line == s.startLine && p.Column >= s.startCol)
	before := p.Line < s.endLine || (p.Line == s.endLine && p.Column < s.endCol)

	return after && before
}

// mutantsIn finds every negatable operator inside the blocks, one mutant each.
func mutantsIn(blocks []attributedBlock) ([]mutant, error) {
	byFile := map[string][]attributedBlock{}
	spans := map[string]span{}

	for _, block := range blocks {
		s, err := parseSpan(block.Block)
		if err != nil {
			return nil, err
		}

		spans[block.Block] = s
		byFile[s.file] = append(byFile[s.file], block)
	}

	var found []mutant

	for file, inFile := range byFile {
		fset := token.NewFileSet()

		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", file, err)
		}

		ast.Inspect(parsed, func(node ast.Node) bool {
			binary, ok := node.(*ast.BinaryExpr)
			if !ok {
				return true
			}

			to, negatable := negations[binary.Op]
			if !negatable {
				return true
			}

			at := fset.Position(binary.OpPos)

			for _, block := range inFile {
				if spans[block.Block].contains(at) {
					found = append(found, mutant{
						File: file, Line: at.Line, Column: at.Column, offset: at.Offset,
						From: binary.Op.String(), To: to.String(), Tests: block.Tests,
					})

					break
				}
			}

			return true
		})
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].File != found[j].File {
			return found[i].File < found[j].File
		}

		return found[i].offset < found[j].offset
	})

	return found, nil
}

// apply is file's source with the mutant's operator swapped.
func (m mutant) apply(source []byte) []byte {
	out := append([]byte{}, source[:m.offset]...)
	out = append(out, m.To...)

	return append(out, source[m.offset+len(m.From):]...)
}

// judge runs every mutant the attribution's e2e-only blocks hold, workers at a time, and writes what killed each.
func judge(ctx context.Context, args []string) error {
	attribution, workers, limit, err := judgeArgs(args)
	if err != nil {
		return err
	}

	blocks, tests, err := readAttribution(attribution)
	if err != nil {
		return err
	}

	seconds := map[string]float64{}
	for _, test := range tests {
		seconds[test.Name] = test.Seconds
	}

	mutants, err := mutantsIn(blocks)
	if err != nil {
		return err
	}

	if limit > 0 && limit < len(mutants) {
		mutants = mutants[:limit]
	}

	work, err := os.MkdirTemp("", "mutants-judge-")
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	runAll(ctx, work, mutants, workers, seconds)

	encoded, err := json.MarshalIndent(mutants, "", " ")
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = os.WriteFile(filepath.Join(filepath.Dir(attribution), "judge.json"), encoded, 0o600) //nolint:gosec // beside the attribution the operator named
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	printVerdicts(mutants, tests)

	return nil
}

// runAll judges the mutants workers at a time, each worker in a directory of its own under work.
func runAll(ctx context.Context, work string, mutants []mutant, workers int, seconds map[string]float64) {
	fmt.Fprintf(os.Stderr, "judging %d mutants with %d workers\n", len(mutants), workers)

	queue := make(chan int)

	var wg sync.WaitGroup

	for worker := range max(workers, 1) {
		wg.Go(func() {
			for i := range queue {
				mutants[i].Outcome, mutants[i].Killers = runMutant(ctx, filepath.Join(work, strconv.Itoa(worker)), mutants[i], seconds)
				fmt.Fprintf(os.Stderr, "%s:%d:%d %s->%s %s %v\n", mutants[i].File, mutants[i].Line, mutants[i].Column, mutants[i].From, mutants[i].To, mutants[i].Outcome, mutants[i].Killers)
			}
		})
	}

	for i := range mutants {
		queue <- i
	}

	close(queue)
	wg.Wait()
}

// judgeArgs are the operands, each optional: the attribution, the workers, and how many mutants to judge.
func judgeArgs(args []string) (string, int, int, error) {
	attribution, numbers := ".coverattr/attribution.json", []int{4, 0}

	if len(args) > 0 {
		attribution = args[0]
	}

	for i, operand := range args[min(len(args), 1):] {
		if i >= len(numbers) {
			break
		}

		n, err := strconv.Atoi(operand)
		if err != nil {
			return "", 0, 0, fmt.Errorf("operand %q: %w", operand, err)
		}

		numbers[i] = n
	}

	return attribution, numbers[0], numbers[1], nil
}

func readAttribution(path string) ([]attributedBlock, []attributedTest, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a path the operator named
	if err != nil {
		return nil, nil, fmt.Errorf("reading the attribution (run `task cover-e2e` first): %w", err)
	}

	var report struct {
		Blocks []attributedBlock `json:"blocks"`
		Tests  []attributedTest  `json:"tests"`
	}

	err = json.Unmarshal(raw, &report)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding %s: %w", path, err)
	}

	return report.Blocks, report.Tests, nil
}

// runMutant builds the e2e binary with the mutant overlaid and runs the tests that execute it: killed by whichever fail, lived if none do.
func runMutant(ctx context.Context, dir string, m mutant, seconds map[string]float64) (string, []string) {
	overlay, err := writeOverlay(dir, m)
	if err != nil {
		return "error: " + err.Error(), nil
	}

	binary := filepath.Join(dir, "e2e.test")

	build := exec.CommandContext(ctx, "go", "test", "-c", "-overlay", overlay, "-o", binary, "./e2e") //nolint:gosec // paths this command made

	out, err := build.CombinedOutput()
	if err != nil {
		return "unbuildable: " + strings.TrimSpace(string(out)), nil
	}

	// Ten times what the tests took unmutated, and a minute besides: a mutant that loops is killed by its timeout, which gremlins scores as killed too.
	budget := time.Minute
	for _, test := range m.Tests {
		budget += time.Duration(10 * seconds[test] * float64(time.Second))
	}

	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	//nolint:gosec // a binary this command built, running tests the attribution named
	test := exec.CommandContext(runCtx, binary, "-test.run", "^("+strings.Join(m.Tests, "|")+")$", "-test.v", "-test.count=1")
	test.Dir = "e2e"
	out, _ = test.CombinedOutput()

	if runCtx.Err() != nil {
		return "timed out", nil
	}

	killers := failedTests(string(out))
	if len(killers) == 0 {
		return "lived", nil
	}

	return "killed", killers
}

// writeOverlay writes the mutated file and the overlay naming it, returning the overlay's path.
func writeOverlay(dir string, m mutant) (string, error) {
	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	source, err := os.ReadFile(m.File)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	mutated := filepath.Join(dir, filepath.Base(m.File))

	err = os.WriteFile(mutated, m.apply(source), 0o600) //nolint:gosec // under this command's own temp directory
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	original, err := filepath.Abs(m.File)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {original: mutated}})
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	path := filepath.Join(dir, "overlay.json")

	err = os.WriteFile(path, overlay, 0o600)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	return path, nil
}

// failedTests are the top-level tests -test.v reports failing; a subtest's line is indented.
func failedTests(output string) []string {
	var failed []string

	for line := range strings.Lines(output) {
		if name, ok := strings.CutPrefix(line, "--- FAIL: "); ok {
			name, _, _ = strings.Cut(name, " ")
			failed = append(failed, name)
		}
	}

	return failed
}

// printVerdicts says, per e2e test, how many mutants it alone noticed: a test that is never the only one to fail can go without losing anything a mutant sees.
func printVerdicts(mutants []mutant, tests []attributedTest) {
	outcomes := map[string]int{}
	alone := map[string]int{}
	noticed := map[string]int{}

	for _, m := range mutants {
		outcome, _, _ := strings.Cut(m.Outcome, ":")
		outcomes[outcome]++

		for _, killer := range m.Killers {
			noticed[killer]++
		}

		if len(m.Killers) == 1 {
			alone[m.Killers[0]]++
		}
	}

	fmt.Printf("%d mutants: %v\n", len(mutants), outcomes)

	var never []string

	for _, test := range tests {
		if alone[test.Name] == 0 {
			never = append(never, test.Name)
		}
	}

	fmt.Printf("%d of %d e2e tests are never the only one to notice a mutant:\n", len(never), len(tests))

	for _, name := range never {
		fmt.Printf("  %s (noticed %d)\n", name, noticed[name])
	}
}
