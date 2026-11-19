package config

import (
	"strings"
	"testing"
)

func TestOnPendingLoadRules(t *testing.T) {
	t.Parallel()

	const head = `
agents:
- name: asker
  source: { model: openai/m, endpoint: "http://127.0.0.1:1/v1/" }
  tools: [ask_user]
- name: teller
  source: { model: openai/m, endpoint: "http://127.0.0.1:1/v1/" }
resources:
- name: repo
  type: git
  source: { uri: "https://example.com/r.git" }
jobs:
- name: release
`

	cases := map[string]struct {
		body, want string
	}{
		"announces through params and env": {body: `
  on_pending:
    agent: teller
    messages: ["((.:pending.message)) ((.:pending.options))"]
  plan:
  - approval: { message: ship }
    on_pending:
      task: dm
      inputs: []
      run: notify "((.:pending.id)) ((.:pending.command)) ((.:pending.url))" "$STEPS_PENDING_MESSAGE"
`},
		"model-written text in a command": {body: `
  plan:
  - approval: { message: ship }
    on_pending:
      task: dm
      inputs: []
      run: notify "((.:pending.message))"
`, want: "read $STEPS_PENDING_MESSAGE instead"},
		"options in a command": {body: `
  on_pending:
    task: dm
    run: notify "((.:pending.options))"
  plan:
  - approval: { message: ship }
`, want: "read $STEPS_PENDING_OPTIONS instead"},
		"the whole park in a command": {body: `
  on_pending:
    task: dm
    run: notify "((.:pending))"
  plan:
  - approval: { message: ship }
`, want: "whole"},
		"a field a park lacks": {body: `
  on_pending:
    task: dm
    run: notify "((.:pending.who))"
  plan:
  - approval: { message: ship }
`, want: "a park has id, kind"},
		"pending outside on_pending": {body: `
  plan:
  - approval: { message: ship }
    on_failure:
      task: dm
      inputs: []
      run: notify "((.:pending.id))"
`, want: "known only inside an on_pending body"},
		"a capture the park would hide": {body: `
  plan:
  - get: repo
  - load_var: pending
    file: repo/x
  - approval: { message: ship }
    on_pending:
      task: dm
      inputs: []
      run: notify "((.:pending.id))"
`, want: "would be hidden"},
		"on a get": {body: `
  plan:
  - get: repo
    on_pending:
      task: dm
      run: "true"
`, want: "on_pending is not valid on a get"},
		"an approval in the body": {body: `
  on_pending:
    approval: { message: again }
  plan:
  - approval: { message: ship }
`, want: "approval: is not valid in a hook"},
		"an ask_user agent in the body": {body: `
  on_pending:
    agent: asker
    messages: [tell someone]
  plan:
  - approval: { message: ship }
`, want: "a park cannot park"},
		"inside another hook": {body: `
  plan:
  - approval: { message: ship }
    on_failure:
      task: dm
      inputs: []
      run: "true"
      on_pending:
        task: again
        run: "true"
`, want: "on_pending is not valid inside a hook"},
		"STEPS_PENDING_ in env": {body: `
  plan:
  - task: t
    inputs: []
    env: [STEPS_PENDING_ID]
    run: "true"
`, want: "set by steps for every step"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, head+tc.body))

			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("LoadConfig: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("LoadConfig = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
