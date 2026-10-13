// Package tabled is analyzer fixture code: config.Step's shape, where Kind()
// ranges over a package-level table whose rows test the fields.
package tabled

type Kind string

type Step struct { // want Step:`kinds\(Get,Task,Try\)`
	Get  string
	Task string
	Try  *Step

	When *string
}

type spec struct {
	kind  Kind
	isSet func(s *Step) bool
}

var kinds = [...]spec{
	{"get", func(s *Step) bool { return s.Get != "" }},
	{"task", func(s *Step) bool { return s.Task != "" }},
	{"try", func(s *Step) bool { return s.Try != nil }},
}

func (s Step) Kind() (Kind, bool) {
	var (
		kind Kind
		ok   bool
	)

	for _, row := range kinds {
		if !row.isSet(&s) {
			continue
		}

		if ok {
			return "", false
		}

		kind, ok = row.kind, true
	}

	return kind, ok
}

func missesTry(step Step) string {
	switch { // want "never tests Try"
	case step.Get != "":
		return "get"
	case step.Task != "":
		return "task"
	}

	return ""
}
