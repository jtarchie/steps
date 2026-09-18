// Package sqlscope finds SQL statements that touch a pipeline-scoped table without naming pipeline_id. It lives in tools/ because it reads source, and its test is what holds internal/store/sqlite to the rule CLAUDE.md states in prose.
package sqlscope

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Finding is one statement on a scoped table that names no pipeline.
type Finding struct {
	Pos   string
	Table string
	SQL   string
}

// Report is what a directory's source says: the findings, and how much was read — so a caller can tell "nothing wrong" from "nothing parsed".
type Report struct {
	Findings   []Finding
	Tables     int
	Statements int
}

var (
	createTable = regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\s*\)`)
	statement   = regexp.MustCompile(`(?i)^\s*(SELECT|INSERT|UPDATE|DELETE|REPLACE|WITH)\b`)
)

// Check reads every non-test Go file in dir. The scoped tables come from the CREATE TABLE statements found there rather than from a list, so a new table is held to the rule the day it is created.
//
// ponytail: "names pipeline_id somewhere", not "scopes every table it touches" — a join of two scoped tables with one predicate passes, and so does a statement whose only mention sits in a shared subquery. Upgrade to a real SQL parser if a review ever finds one of those; every unscoped statement found so far named it nowhere.
func Check(dir string) (Report, error) {
	texts, err := sqlTexts(dir)
	if err != nil {
		return Report{}, err
	}

	scoped := map[string]*regexp.Regexp{}

	for _, sql := range texts {
		for _, match := range createTable.FindAllStringSubmatch(sql.text, -1) {
			if strings.Contains(match[2], "pipeline_id") {
				scoped[match[1]] = regexp.MustCompile(`\b` + match[1] + `\b`)
			}
		}
	}

	report := Report{Tables: len(scoped)}

	for _, sql := range texts {
		if !statement.MatchString(sql.text) {
			continue
		}

		report.Statements++

		if strings.Contains(sql.text, "pipeline_id") {
			continue
		}

		for table, named := range scoped {
			if named.MatchString(sql.text) {
				report.Findings = append(report.Findings, Finding{Pos: sql.pos, Table: table, SQL: sql.text})
			}
		}
	}

	return report, nil
}

type sqlText struct {
	pos  string
	text string
}

// sqlTexts is every string the package builds, with `const + literal` concatenations folded first: a statement split around a shared column list names its pipeline in the half the other half does not contain.
func sqlTexts(dir string) ([]sqlText, error) {
	fset := token.NewFileSet()

	files, err := parseDir(fset, dir)
	if err != nil {
		return nil, err
	}

	folder := folder{consts: constsOf(files)}

	var texts []sqlText

	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			// A const declaration is read where it is USED, folded into the statement it completes; on its own it is half a sentence.
			if gen, ok := node.(*ast.GenDecl); ok && gen.Tok == token.CONST {
				return false
			}

			expr, ok := node.(ast.Expr)
			if !ok {
				return true
			}

			text, ok := folder.fold(expr, 0)
			if !ok {
				return true
			}

			texts = append(texts, sqlText{pos: fset.Position(expr.Pos()).String(), text: text})

			// The maximal expression has been read; its parts are not statements of their own.
			return false
		})
	}

	return texts, nil
}

func parseDir(fset *token.FileSet, dir string) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var files []*ast.File

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}

		files = append(files, file)
	}

	return files, nil
}

func constsOf(files []*ast.File) map[string]ast.Expr {
	consts := map[string]ast.Expr{}

	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}

			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}

				for i, name := range value.Names {
					if i < len(value.Values) {
						consts[name.Name] = value.Values[i]
					}
				}
			}
		}
	}

	return consts
}

type folder struct {
	consts map[string]ast.Expr
}

// maxConstDepth bounds const-to-const references, which the compiler already forbids from cycling; the bound is for a file that does not compile.
const maxConstDepth = 8

func (f folder) fold(expr ast.Expr, depth int) (string, bool) {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}

		text, err := strconv.Unquote(node.Value)

		return text, err == nil
	case *ast.Ident:
		value, ok := f.consts[node.Name]
		if !ok || depth > maxConstDepth {
			return "", false
		}

		return f.fold(value, depth+1)
	case *ast.ParenExpr:
		return f.fold(node.X, depth)
	case *ast.BinaryExpr:
		return f.concat(node, depth)
	default:
		return "", false
	}
}

// concat folds a + b. A half that is not constant (a placeholder list built at run time) becomes a marker, so the constant halves around it are still read as ONE statement.
func (f folder) concat(node *ast.BinaryExpr, depth int) (string, bool) {
	if node.Op != token.ADD {
		return "", false
	}

	left, leftOK := f.fold(node.X, depth)
	right, rightOK := f.fold(node.Y, depth)

	if !leftOK && !rightOK {
		return "", false
	}

	if !leftOK {
		left = " ? "
	}

	if !rightOK {
		right = " ? "
	}

	return left + right, true
}
