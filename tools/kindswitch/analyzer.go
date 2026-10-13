// Package kindswitch keeps step-kind dispatch where golangci-lint's
// `exhaustive` can check it.
//
// exhaustive checks `switch kind, _ := step.Kind(); kind { ... }`: adding a
// StepKind makes every such switch that does not answer it fail lint. It
// cannot see the tagless spelling, `switch { case step.Get != "": ... }` —
// that is `switch true` over boolean cases, with no enum to reason about —
// and adding a kind once shipped six defects that were tagless sites silently
// doing nothing for it.
//
// So this pass reports any tagless switch whose cases read a struct field
// tagged `kind:"..."` (config.Step's kind fields). The tag, not a list here,
// is what names a kind field.
//
// Usage:
//
//	go run ./tools/kindswitch ./...
package main

import (
	"go/ast"
	"go/types"
	"reflect"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/analysis/singlechecker"
	"golang.org/x/tools/go/ast/inspector"
)

func main() { singlechecker.Main(Analyzer) }

// ignoreDirective suppresses a report on the switch it precedes or trails.
// A bare directive is not honored: the reason is the point.
const ignoreDirective = "//kindswitch:ignore"

// Analyzer is the go/analysis pass.
var Analyzer = &analysis.Analyzer{
	Name:     "kindswitch",
	Doc:      "report tagless switches over kind-tagged fields, which exhaustive cannot check",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	inspected, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return nil, nil //nolint:nilnil // an analysis pass with no result type returns nil, nil
	}

	ignored := ignoredLines(pass)

	inspected.Preorder([]ast.Node{(*ast.SwitchStmt)(nil)}, func(node ast.Node) {
		stmt, ok := node.(*ast.SwitchStmt)
		if !ok || stmt.Tag != nil {
			return
		}

		field := kindFieldTested(pass, stmt)
		if field == "" {
			return
		}

		position := pass.Fset.Position(stmt.Switch)
		if ignored[ignoreKey{file: position.Filename, line: position.Line}] {
			return
		}

		pass.Reportf(stmt.Switch,
			"tagless switch tests kind field %s; dispatch on the kind with `switch kind, _ := s.Kind(); kind` so exhaustive checks it, or suppress with `%s <reason>`",
			field, ignoreDirective)
	})

	return nil, nil //nolint:nilnil // an analysis pass with no result type returns nil, nil
}

// kindFieldTested names the first kind-tagged field any case expression of
// stmt reads, or "".
func kindFieldTested(pass *analysis.Pass, stmt *ast.SwitchStmt) string {
	var found string

	for _, clause := range stmt.Body.List {
		caseClause, ok := clause.(*ast.CaseClause)
		if !ok {
			continue
		}

		for _, expr := range caseClause.List {
			ast.Inspect(expr, func(node ast.Node) bool {
				sel, ok := node.(*ast.SelectorExpr)
				if !ok || found != "" {
					return found == ""
				}

				if isKindField(pass.TypesInfo.Selections[sel]) {
					found = sel.Sel.Name
				}

				return found == ""
			})
		}
	}

	return found
}

// isKindField reports whether selection reads a struct field carrying a
// `kind:` tag, following the index path through embedded fields.
func isKindField(selection *types.Selection) bool {
	if selection == nil || selection.Kind() != types.FieldVal {
		return false
	}

	typ := selection.Recv()

	var tag string

	for _, index := range selection.Index() {
		structType, ok := deref(typ).Underlying().(*types.Struct)
		if !ok || index >= structType.NumFields() {
			return false
		}

		tag = structType.Tag(index)
		typ = structType.Field(index).Type()
	}

	_, ok := reflect.StructTag(tag).Lookup("kind")

	return ok
}

func deref(typ types.Type) types.Type {
	if pointer, ok := types.Unalias(typ).(*types.Pointer); ok {
		return pointer.Elem()
	}

	return typ
}

type ignoreKey struct {
	file string
	line int
}

// ignoredLines collects the lines a `//kindswitch:ignore <reason>` comment
// covers: its own line and the next.
func ignoredLines(pass *analysis.Pass) map[ignoreKey]bool {
	ignored := map[ignoreKey]bool{}

	for _, file := range pass.Files {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				reason, ok := strings.CutPrefix(comment.Text, ignoreDirective)
				if !ok || strings.TrimSpace(reason) == "" {
					continue
				}

				position := pass.Fset.Position(comment.Pos())
				ignored[ignoreKey{file: position.Filename, line: position.Line}] = true
				ignored[ignoreKey{file: position.Filename, line: position.Line + 1}] = true
			}
		}
	}

	return ignored
}
