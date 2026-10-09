package config

import (
	"strings"
	"testing"
)

func TestRouteBoundaries(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pipeline string
		wantErr  string
	}{
		"a step routing to itself is backward": {`
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
    to: { failure: a }
`, "max_visits must be set"},
		"max_visits at the limit loads": {`
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
    to: { failure: a }
    max_visits: 1000
`, ""},
		"a routing error before a get is still reported": {`
resources:
- name: src
  type: git
  source: { uri: https://example.com/repo.git }
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
    to: { success: nowhere }
  - get: src
`, `routes to "nowhere"`},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, test.pipeline))

			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("LoadConfig: %v", err)
			case test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)):
				t.Fatalf("LoadConfig error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}
