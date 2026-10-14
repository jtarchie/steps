package template

import (
	"slices"
	"text/template"
	"text/template/parse"
)

// UnquotedRefs reports each action in tmpl that prints a value derived from
// one of roots (".version", ".args", ...) without ending in shellquote — the
// spots where a value reaches `sh -c` as shell syntax rather than as a word.
// Each is returned once, as text/template prints the action. An unparsable
// template reports nothing: rendering it fails louder than this could.
//
// A value is derived from a root when it is reached through a field of it,
// through a dot rebound by with/range over it, or through a variable assigned
// from it. ponytail: variables are tracked by name across the whole template,
// not by scope — a shadowing $x can be over- or under-reported.
func UnquotedRefs(tmpl string, roots ...string) []string {
	parsed, err := template.New("scan").Funcs(funcMap).Parse(tmpl)
	if err != nil {
		return nil
	}

	scan := &refScan{roots: roots, tainted: map[string]bool{}}

	for _, t := range parsed.Templates() {
		if t.Tree != nil {
			scan.walk(t.Root, false)
		}
	}

	return scan.found
}

type refScan struct {
	roots   []string
	tainted map[string]bool
	found   []string
}

// walk visits node; dotTainted says whether "." is currently a root-derived
// value (inside with/range over one).
func (s *refScan) walk(node parse.Node, dotTainted bool) {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}

		for _, child := range n.Nodes {
			s.walk(child, dotTainted)
		}
	case *parse.ActionNode:
		s.action(n, dotTainted)
	case *parse.IfNode:
		s.walk(n.List, dotTainted)
		s.walk(n.ElseList, dotTainted)
	case *parse.WithNode:
		s.declare(n.Pipe, dotTainted)
		s.walk(n.List, s.pipeTainted(n.Pipe, dotTainted))
		s.walk(n.ElseList, dotTainted)
	case *parse.RangeNode:
		s.declare(n.Pipe, dotTainted)
		s.walk(n.List, s.pipeTainted(n.Pipe, dotTainted))
		s.walk(n.ElseList, dotTainted)
	}
}

func (s *refScan) action(n *parse.ActionNode, dotTainted bool) {
	if len(n.Pipe.Decl) > 0 {
		s.declare(n.Pipe, dotTainted)

		return
	}

	if endsInShellquote(n.Pipe) || !s.pipeTainted(n.Pipe, dotTainted) {
		return
	}

	expr := n.String()
	if !slices.Contains(s.found, expr) {
		s.found = append(s.found, expr)
	}
}

// declare marks the variables a pipe assigns as tainted when its value is.
func (s *refScan) declare(pipe *parse.PipeNode, dotTainted bool) {
	if pipe == nil || !s.pipeTainted(pipe, dotTainted) {
		return
	}

	for _, v := range pipe.Decl {
		s.tainted[v.Ident[0]] = true
	}
}

func endsInShellquote(pipe *parse.PipeNode) bool {
	if len(pipe.Cmds) == 0 {
		return false
	}

	last := pipe.Cmds[len(pipe.Cmds)-1]
	if len(last.Args) == 0 {
		return false
	}

	ident, ok := last.Args[0].(*parse.IdentifierNode)

	return ok && ident.Ident == "shellquote"
}

func (s *refScan) pipeTainted(pipe *parse.PipeNode, dotTainted bool) bool {
	if pipe == nil {
		return false
	}

	for _, cmd := range pipe.Cmds {
		for _, arg := range cmd.Args {
			if s.argTainted(arg, dotTainted) {
				return true
			}
		}
	}

	return false
}

func (s *refScan) argTainted(arg parse.Node, dotTainted bool) bool {
	switch a := arg.(type) {
	case *parse.DotNode:
		return dotTainted
	case *parse.FieldNode:
		return dotTainted || slices.Contains(s.roots, a.Ident[0])
	case *parse.ChainNode:
		return s.argTainted(a.Node, dotTainted)
	case *parse.VariableNode:
		if a.Ident[0] == "$" {
			return len(a.Ident) > 1 && slices.Contains(s.roots, a.Ident[1])
		}

		return s.tainted[a.Ident[0]]
	case *parse.PipeNode:
		return s.pipeTainted(a, dotTainted)
	}

	return false
}
