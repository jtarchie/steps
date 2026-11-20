package config

import (
	"slices"
	"strings"
	"testing"
)

func extraHostsPipeline(task string) string {
	return `
tasks:
- name: probe
  run: "true"
` + task + `
jobs:
- name: j
  plan:
  - task: probe
`
}

func TestExtraHostsMapIsSortedAndListKeepsItsOrder(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		yaml string
		want []string
	}{
		"map":  {"  image: alpine\n  extra_hosts:\n    zeta.test: 10.0.0.2\n    alpha.test: host-gateway\n", []string{"alpha.test:host-gateway", "zeta.test:10.0.0.2"}},
		"list": {"  image: alpine\n  extra_hosts: [\"zeta.test:10.0.0.2\", \"alpha.test:::1\"]\n", []string{"zeta.test:10.0.0.2", "alpha.test:::1"}},
	}

	for name, tc := range cases {
		cfg := loadOK(t, extraHostsPipeline(tc.yaml))

		if got := cfg.Tasks[0].ExtraHosts; !slices.Equal(got, tc.want) {
			t.Errorf("%s: ExtraHosts = %q, want %q", name, got, tc.want)
		}

		resolved, err := cfg.ResolveTask(cfg.Jobs[0].Plan[0])
		if err != nil {
			t.Fatal(err)
		}

		if !slices.Equal(resolved.ExtraHosts, tc.want) {
			t.Errorf("%s: resolved ExtraHosts = %q, want the entry's %q", name, resolved.ExtraHosts, tc.want)
		}
	}
}

func TestExtraHostsEmptyIsUnset(t *testing.T) {
	t.Parallel()

	for _, empty := range []string{"{}", "[]"} {
		cfg := loadOK(t, extraHostsPipeline("  image: alpine\n  extra_hosts: "+empty+"\n"))

		if got := cfg.Tasks[0].ExtraHosts; got != nil {
			t.Errorf("extra_hosts: %s = %#v, want nil, which a sub-agent's sameContainer compares unset against", empty, got)
		}
	}
}

func TestExtraHostsRefusals(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		pipeline string
		want     string
	}{
		"scalar":                 {extraHostsPipeline("  image: alpine\n  extra_hosts: a.test:10.0.0.1\n"), "a map of name: address or a list"},
		"map value not a scalar": {extraHostsPipeline("  image: alpine\n  extra_hosts: {a.test: [10.0.0.1]}\n"), "each value is an address"},
		"no colon":               {extraHostsPipeline("  image: alpine\n  extra_hosts: [a.test]\n"), `"a.test" must be name:address`},
		"empty name":             {extraHostsPipeline("  image: alpine\n  extra_hosts: [\":10.0.0.1\"]\n"), "must be name:address"},
		"empty address":          {extraHostsPipeline("  image: alpine\n  extra_hosts: {a.test: \"\"}\n"), "must be name:address"},
		"not an address":         {extraHostsPipeline("  image: alpine\n  extra_hosts: {a.test: example.com}\n"), "neither an IP address nor host-gateway"},
		"task on the host":       {extraHostsPipeline("  extra_hosts: {a.test: host-gateway}\n"), `task "probe": extra_hosts requires image:`},
		"task beside no network": {extraHostsPipeline("  image: alpine\n  network: none\n  extra_hosts: {a.test: host-gateway}\n"), `task "probe": extra_hosts beside network: none`},
		"agent on the host": {`
agents:
- name: a
  source: {model: lmstudio/qwen}
  extra_hosts: {a.test: host-gateway}
jobs:
- name: j
  plan:
  - agent: a
    messages: [hi]
`, `agent "a": extra_hosts requires image:`},
		"resource_type beside no network": {`
resource_types:
- name: rt
  image: alpine
  network: none
  extra_hosts: ["a.test:10.0.0.1"]
  config:
    check: echo '[]'
resources:
- name: r
  type: rt
jobs:
- name: j
  plan:
  - get: r
`, `resource_type "rt": extra_hosts beside network: none`},
		"step cuts a task's network": {`
tasks:
- name: probe
  run: "true"
  image: alpine
  extra_hosts: {a.test: host-gateway}
jobs:
- name: j
  plan:
  - task: probe
    network: none
`, "network: none over an entry with extra_hosts"},
		"step cuts an agent's network": {`
agents:
- name: a
  source: {model: lmstudio/qwen}
  image: alpine
  extra_hosts: {a.test: host-gateway}
jobs:
- name: j
  plan:
  - agent: a
    network: none
    messages: [hi]
`, "network: none over an entry with extra_hosts"},
	}

	for name, tc := range cases {
		_, err := LoadConfig(writeConfig(t, tc.pipeline))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: LoadConfig error = %v, want it to contain %q", name, err, tc.want)
		}
	}
}

func TestExtraHostsStepNetworkOtherThanNoneIsAccepted(t *testing.T) {
	t.Parallel()

	loadOK(t, `
tasks:
- name: probe
  run: "true"
  image: alpine
  extra_hosts: {a.test: host-gateway}
jobs:
- name: j
  plan:
  - task: probe
    network: bridge
`)
}
