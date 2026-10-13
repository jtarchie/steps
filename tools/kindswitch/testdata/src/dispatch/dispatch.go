// Package dispatch is analyzer fixture code: a miniature of config.Step and
// the tagless switches the analyzer must and must not report.
package dispatch

type Kind string

const (
	KindGet  Kind = "get"
	KindTask Kind = "task"
)

type Step struct {
	Get  string `yaml:"get" kind:"get"`
	Task string `yaml:"task" kind:"task"`
	Try  *Step  `kind:"try"`

	When *string `yaml:"when"`
}

func (s Step) Kind() (Kind, bool) {
	if s.Get != "" {
		return KindGet, true
	}

	return "", false
}

// wrapper reaches the kind fields through an embedded Step.
type wrapper struct {
	Step
}

func taglessOnKind(step Step) string {
	switch { // want "tagless switch tests kind field Task"
	case step.Task != "":
		return "task"
	}

	return ""
}

func compoundCase(step Step, allow bool) string {
	switch { // want "tagless switch tests kind field Get"
	case step.Get != "" && !allow:
		return "get"
	}

	return ""
}

func mixedWithModifier(step Step) string {
	switch { // want "tagless switch tests kind field Try"
	case step.When != nil:
		return "when"
	case step.Try != nil:
		return "try"
	}

	return ""
}

func throughEmbedding(w wrapper) string {
	switch { // want "tagless switch tests kind field Get"
	case w.Get != "":
		return "get"
	}

	return ""
}

func suppressed(step Step) string {
	//kindswitch:ignore the fixture's reason
	switch {
	case step.Get != "":
		return "get"
	}

	return ""
}

func trailingSuppression(step Step) string {
	switch { //kindswitch:ignore the fixture's reason
	case step.Get != "":
		return "get"
	}

	return ""
}

func bareDirectiveIsNotSuppression(step Step) string {
	//kindswitch:ignore
	switch { // want "tagless switch tests kind field Get"
	case step.Get != "":
		return "get"
	}

	return ""
}

// Untagged fields are not kinds.
func modifierOnly(step Step) string {
	switch {
	case step.When != nil:
		return "when"
	}

	return ""
}

func ordinaryTaglessSwitch(n int) string {
	switch {
	case n < 0:
		return "negative"
	case n == 0:
		return "zero"
	}

	return "positive"
}

// Tagged dispatch is exhaustive's job.
func taggedSwitch(step Step) string {
	switch kind, _ := step.Kind(); kind {
	case KindGet:
		return "get"
	case KindTask:
		return "task"
	}

	return ""
}

// A kind field read in a case BODY is not dispatch.
func fieldInBody(step Step, n int) string {
	switch {
	case n > 0:
		return step.Get
	}

	return ""
}
