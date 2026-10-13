// Package unreadable has a Kind() whose table kindswitch cannot read: the kinds
// come from a function call, not from fields the body or a package var tests.
package unreadable

type Kind string

type Step struct {
	Get  string
	Task string
}

func lookup(s Step) (Kind, bool) {
	if s.Get != "" {
		return "get", true
	}

	return "", false
}

func (s Step) Kind() (Kind, bool) { // want `kindswitch cannot read the kinds this Kind\(\) tests`
	return lookup(s)
}
