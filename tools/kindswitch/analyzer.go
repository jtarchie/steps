// Package kindswitch reports tagless kind dispatch that silently ignores a
// step kind.
//
// golangci-lint's `exhaustive` already catches the tagged spelling:
//
//	switch kind {          // exhaustive CAN see this
//	case config.StepKindPut:
//	}
//
// It cannot see the other half of how this codebase dispatches:
//
//	switch {               // exhaustive CANNOT see this
//	case step.Put != "":
//	case step.Get != "":
//	}
//
// The tagless form is `switch true`: the tag is an untyped bool and the cases
// are boolean expressions, so there is no enum type for the analyzer to reason
// about. That is not a configuration gap, it is outside exhaustive's model —
// verified empirically, not assumed. The consequence is not hypothetical
// either: adding one StepKind once shipped six defects that were a dispatch
// site handling every other kind and silently doing nothing for the new one,
// and one of the misses (workspace.validateStepArtifactFlow, a tagless site)
// made a whole job statically unrunnable.
//
// The kind table is not hardcoded here. It is read out of the type's own
// Kind() method — the same table Kind() dispatches on — and published as an
// analysis Fact, so adding a kind to that table is the single edit that
// widens what this checks.
//
// Usage:
//
//	go run ./tools/kindswitch ./...
package main

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/analysis/singlechecker"
	"golang.org/x/tools/go/ast/inspector"
)

func main() { singlechecker.Main(Analyzer) }

// ignoreDirective suppresses a report on the switch it precedes (or sits on
// the same line as). A reason is mandatory — an unexplained suppression is the
// state this analyzer exists to end, since the current tagless sites are
// neither linted nor suppressed and therefore read as covered.
const ignoreDirective = "//kindswitch:ignore"

// Analyzer is the go/analysis pass. Run it over the module:
//
//	go run ./tools/kindswitch ./...
var Analyzer = &analysis.Analyzer{
	Name:      "kindswitch",
	Doc:       "report tagless kind dispatch that omits a kind from the type's own Kind() table",
	Requires:  []*analysis.Analyzer{inspect.Analyzer},
	FactTypes: []analysis.Fact{(*kindTable)(nil)},
	Run:       run,
}

// kindTable is the set of fields a type's Kind() method tests, in declaration
// order, attached as a Fact to that type so importing packages can read it.
type kindTable struct {
	Fields []string
}

func (*kindTable) AFact() {}

func (k *kindTable) String() string { return "kinds(" + strings.Join(k.Fields, ",") + ")" }

func run(pass *analysis.Pass) (any, error) {
	publishKindTables(pass)
	checkTaglessSwitches(pass)

	return nil, nil //nolint:nilnil // an analysis pass with no result type returns nil, nil
}

// publishKindTables finds each `func (s T) Kind() (..., bool)` in this package
// and exports the fields its body tests as a Fact on T.
func publishKindTables(pass *analysis.Pass) {
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			named, fields := kindTableOf(pass, fn)
			if named != nil {
				pass.ExportObjectFact(named, &kindTable{Fields: fields})
			}
		}
	}
}

// kindTableOf recognizes a kind-table method — `func (s T) Kind() (K, bool)`
// testing two or more of its receiver's fields — and returns T with the fields
// its body tests. Anything else yields a nil type.
func kindTableOf(pass *analysis.Pass, fn *ast.FuncDecl) (*types.TypeName, []string) {
	if fn.Name.Name != "Kind" || fn.Recv == nil || fn.Body == nil {
		return nil, nil
	}

	if fn.Type.Results == nil || fn.Type.Results.NumFields() != 2 {
		return nil, nil
	}

	recv := receiverName(fn)
	if recv == "" {
		return nil, nil
	}

	fields := selectedFields(fn.Body, recv)
	if len(fields) < 2 {
		return nil, nil // not a dispatch table, whatever else it is
	}

	return receiverTypeName(pass, fn), fields
}

// receiverName is the receiver's identifier, or "" for an unnamed receiver.
func receiverName(fn *ast.FuncDecl) string {
	names := fn.Recv.List[0].Names
	if len(names) == 0 || names[0].Name == "_" {
		return ""
	}

	return names[0].Name
}

// receiverTypeName resolves the TypeName object a method is declared on,
// looking through a pointer receiver.
func receiverTypeName(pass *analysis.Pass, fn *ast.FuncDecl) *types.TypeName {
	obj, ok := pass.TypesInfo.Defs[fn.Name].(*types.Func)
	if !ok {
		return nil
	}

	sig, ok := obj.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil
	}

	named, ok := types.Unalias(deref(sig.Recv().Type())).(*types.Named)
	if !ok {
		return nil
	}

	return named.Obj()
}

// selectedFields lists, in first-seen order, the field names a body selects off
// the named receiver — `s.Get`, `s.Try`, and so on.
func selectedFields(body *ast.BlockStmt, recv string) []string {
	var (
		fields []string
		seen   = map[string]bool{}
	)

	ast.Inspect(body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != recv || seen[sel.Sel.Name] {
			return true
		}

		seen[sel.Sel.Name] = true
		fields = append(fields, sel.Sel.Name)

		return true
	})

	return fields
}

// checkTaglessSwitches reports every `switch { case v.Field ... }` over a
// kind-table type that never tests one of the table's fields.
func checkTaglessSwitches(pass *analysis.Pass) {
	inspected, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return
	}

	ignored := ignoredLines(pass)

	inspected.Preorder([]ast.Node{(*ast.SwitchStmt)(nil)}, func(node ast.Node) {
		stmt, ok := node.(*ast.SwitchStmt)
		if !ok || stmt.Tag != nil {
			return // tagged: exhaustive already covers it
		}

		subject, tested := switchSubject(pass, stmt)
		if subject == nil {
			return
		}

		table := new(kindTable)
		if !pass.ImportObjectFact(subject, table) {
			return // not a type with a Kind() table — an ordinary tagless switch
		}

		// Scoping: every case must test kind fields and nothing else. A switch
		// mixing kind fields with other conditions (pipeline's
		// unskippableReason tests Put/Agent/Try alongside When/To) is not kind
		// dispatch, and reporting the kinds it "omits" would be wrong.
		for field := range tested {
			if !contains(table.Fields, field) {
				return
			}
		}

		missing := missingFields(table.Fields, tested)
		if len(missing) == 0 {
			return
		}

		position := pass.Fset.Position(stmt.Switch)
		if ignored[ignoreKey{file: position.Filename, line: position.Line}] {
			return
		}

		pass.Reportf(stmt.Switch,
			"tagless dispatch on %s never tests %s (in %s's Kind() table); handle it, or suppress with `%s <reason>`",
			subject.Name(), strings.Join(missing, ", "), subject.Name(), ignoreDirective)
	})
}

// switchSubject identifies the single variable a tagless switch dispatches on,
// plus the set of its fields the cases test. It returns nil unless every
// non-default case tests at least one field of exactly one variable — the
// scoping that keeps this off ordinary tagless switches, which are common and
// fine.
func switchSubject(pass *analysis.Pass, stmt *ast.SwitchStmt) (*types.TypeName, map[string]bool) {
	var (
		variable types.Object
		tested   = map[string]bool{}
	)

	for _, clause := range stmt.Body.List {
		caseClause, ok := clause.(*ast.CaseClause)
		if !ok || len(caseClause.List) == 0 {
			continue // default: doesn't signify exhaustive here, matching exhaustive's own default
		}

		owner, fields := caseSubject(pass, caseClause.List)
		if owner == nil || (variable != nil && variable != owner) {
			return nil, nil // tests nothing about the subject, or a second variable
		}

		variable = owner

		for _, field := range fields {
			tested[field] = true
		}
	}

	if variable == nil {
		return nil, nil
	}

	named, ok := types.Unalias(deref(variable.Type())).(*types.Named)
	if !ok {
		return nil, nil
	}

	return named.Obj(), tested
}

// caseSubject reports the single variable one case clause tests fields of,
// plus those field names. A clause touching no field, or fields of more than
// one variable, yields a nil object.
func caseSubject(pass *analysis.Pass, exprs []ast.Expr) (types.Object, []string) {
	var (
		owner  types.Object
		fields []string
		mixed  bool
	)

	for _, expr := range exprs {
		ast.Inspect(expr, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			found := fieldOwner(pass, sel)
			if found == nil {
				return true
			}

			if owner != nil && owner != found {
				mixed = true

				return false
			}

			owner = found
			fields = append(fields, sel.Sel.Name)

			return true
		})
	}

	if mixed {
		return nil, nil
	}

	return owner, fields
}

// fieldOwner resolves the VARIABLE a selector reads a field from, or nil when
// the selector isn't a field read on a plain variable. The variable, not its
// type: two Step-typed variables in one switch are two dispatches, and
// answering with the type would silently merge them.
func fieldOwner(pass *analysis.Pass, sel *ast.SelectorExpr) types.Object {
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil
	}

	variable, ok := pass.TypesInfo.Uses[ident].(*types.Var)
	if !ok {
		return nil
	}

	if _, ok := pass.TypesInfo.Selections[sel]; !ok {
		return nil // a qualified identifier (pkg.Name), not a field
	}

	if _, ok := types.Unalias(deref(variable.Type())).(*types.Named); !ok {
		return nil
	}

	return variable
}

// missingFields lists table entries no case tested, preserving table order so
// the message reads in the same order the type declares its kinds.
func missingFields(table []string, tested map[string]bool) []string {
	var missing []string

	for _, field := range table {
		if !tested[field] {
			missing = append(missing, field)
		}
	}

	return missing
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func deref(typ types.Type) types.Type {
	pointer, ok := types.Unalias(typ).(*types.Pointer)
	if !ok {
		return typ
	}

	return pointer.Elem()
}

// ignoreKey addresses one suppressible line.
type ignoreKey struct {
	file string
	line int
}

// ignoredLines collects the lines a `//kindswitch:ignore <reason>` comment
// suppresses: the comment's own line (trailing form) and the line after it
// (preceding form).
//
// The reason is mandatory, enforced by simply not honoring a bare directive —
// the switch keeps reporting until someone writes down why the omitted kind is
// deliberate. An unexplained suppression would recreate the state this
// analyzer exists to end, where a dispatch site that ignores a kind is neither
// checked nor marked and therefore reads as covered.
func ignoredLines(pass *analysis.Pass) map[ignoreKey]bool {
	ignored := map[ignoreKey]bool{}

	for _, file := range pass.Files {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if !strings.HasPrefix(comment.Text, ignoreDirective) {
					continue
				}

				if strings.TrimSpace(strings.TrimPrefix(comment.Text, ignoreDirective)) == "" {
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
