package config

import (
	"strings"
	"testing"
)

func TestAskStepRefusesWhatNobodyCouldAnswer(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ step, want string }{
		"no question":           {`{ask: bump}`, "needs a question"},
		"a dotted name":         {`{ask: a.b, question: q}`, "must contain only"},
		"an empty option":       {`{ask: bump, question: q, options: [major, ""]}`, "options[1] is empty"},
		"an option twice":       {`{ask: bump, question: q, options: [major, major]}`, "offers \"major\" twice"},
		"a default not offered": {`{ask: bump, question: q, options: [major], default: minor}`, "is not one of its options"},
		"question without ask":  {`{task: t, run: "true", question: q}`, "question: without ask:"},
		"options without ask":   {`{task: t, run: "true", options: [a]}`, "options: without ask:"},
		"default without ask":   {`{task: t, run: "true", default: a}`, "default: without ask:"},
		"in a hook":             {`{task: t, run: "true", ensure: {ask: bump, question: q}}`, "ask: is not valid in a hook"},
		"one name asked twice": {`{ask: note, question: q}
  - {ask: note, question: again}`, `ask "note" is already the name of an earlier ask:`},
		"free text read as a capture": {`{ask: note, question: q}
  - task: use
    run: echo ((.:note))`, "references ((.:note))"},
	} {
		_, err := LoadConfig(writeConfig(t, `
jobs:
- name: release
  plan:
  - `+tc.step+`
`))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: LoadConfig = %v, want an error saying %q", name, err, tc.want)
		}
	}
}

func TestAskStepWithOptionsIsCaptured(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(writeConfig(t, `
jobs:
- name: release
  plan:
  - ask: bump
    question: Which bump?
    options: [major, minor]
    default: minor
  - task: use
    inputs: [bump]
    run: echo ((.:bump)) $(cat bump/answer)
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}
