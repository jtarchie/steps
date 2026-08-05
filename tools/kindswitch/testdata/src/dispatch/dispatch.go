// Package dispatch is analyzer fixture code: a miniature of config.Step and
// the shapes of tagless switch this analyzer must and must not report.
package dispatch

type Kind string

const (
	KindGet  Kind = "get"
	KindTask Kind = "task"
	KindPut  Kind = "put"
	KindTry  Kind = "try"
)

type Step struct { // want Step:`kinds\(Get,Task,Put,Try\)`
	Get  string
	Task string
	Put  string
	Try  *Step

	// Not a kind field: present so a switch can mix kind and non-kind cases.
	When *string
}

// Kind is the table the analyzer reads. Its four field tests ARE the kind set.
func (s Step) Kind() (Kind, bool) {
	var (
		kind Kind
		ok   bool
	)

	for _, candidate := range [...]struct {
		kind Kind
		set  bool
	}{
		{KindGet, s.Get != ""},
		{KindTask, s.Task != ""},
		{KindPut, s.Put != ""},
		{KindTry, s.Try != nil},
	} {
		if !candidate.set {
			continue
		}

		if ok {
			return "", false
		}

		kind, ok = candidate.kind, true
	}

	return kind, ok
}

// missesTwoKinds is the defect shape: handles some kinds, silently does
// nothing for the rest.
func missesTwoKinds(step Step) string {
	switch { // want "never tests Put, Try"
	case step.Get != "":
		return "get"
	case step.Task != "":
		return "task"
	}

	return ""
}

// aDefaultIsNotCoverage matches golangci-lint's own exhaustive setting, where
// a default clause does not make a switch exhaustive.
func aDefaultIsNotCoverage(step Step) string {
	switch { // want "never tests Try"
	case step.Get != "":
		return "get"
	case step.Task != "":
		return "task"
	case step.Put != "":
		return "put"
	default:
		return "?"
	}
}

// handlesEveryKind is clean.
func handlesEveryKind(step Step) string {
	switch {
	case step.Get != "":
		return "get"
	case step.Task != "":
		return "task"
	case step.Put != "":
		return "put"
	case step.Try != nil:
		return "try"
	}

	return ""
}

// suppressed states why a kind is skipped, so it is not reported.
func suppressed(step Step) string {
	//kindswitch:ignore Try cannot reach here, the caller unwraps first
	switch {
	case step.Get != "":
		return "get"
	case step.Task != "":
		return "task"
	case step.Put != "":
		return "put"
	}

	return ""
}

// bareDirectiveIsNotSuppression: a directive with no reason keeps reporting.
func bareDirectiveIsNotSuppression(step Step) string {
	//kindswitch:ignore
	switch { // want "never tests Put, Try"
	case step.Get != "":
		return "get"
	case step.Task != "":
		return "task"
	}

	return ""
}

// notKindDispatch mixes a kind field with an unrelated one, so it is not a
// dispatch over the kind table and reporting its "missing" kinds would be
// wrong. (This is internal/pipeline's unskippableReason in miniature.)
func notKindDispatch(step Step) string {
	switch {
	case step.Put != "":
		return "put step"
	case step.When != nil:
		return "when: guard"
	}

	return ""
}

// twoVariables is not one dispatch.
func twoVariables(a, b Step) string {
	switch {
	case a.Get != "":
		return "a"
	case b.Task != "":
		return "b"
	}

	return ""
}

// ordinaryTaglessSwitch has nothing to do with a kind table. These are common
// and fine, and the analyzer must stay off them.
func ordinaryTaglessSwitch(n int) string {
	switch {
	case n < 0:
		return "negative"
	case n == 0:
		return "zero"
	}

	return "positive"
}

// taggedSwitch is exhaustive's job, not this analyzer's.
func taggedSwitch(step Step) string {
	kind, _ := step.Kind()

	switch kind {
	case KindGet:
		return "get"
	}

	return ""
}
