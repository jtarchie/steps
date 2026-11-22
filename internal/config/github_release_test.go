package config

import (
	"strings"
	"testing"
)

// TestGitHubReleaseLoadRules: a github-releases source is refused at load for anything its check could only get wrong at run time, and a Concourse spelling is answered with the steps one.
func TestGitHubReleaseLoadRules(t *testing.T) {
	t.Parallel()

	pipeline := func(source, plan string) string {
		if plan == "" {
			plan = "\n  - get: release"
		}

		return `
resources:
- name: release
  type: github-releases
  source:
    repo: acme/app
` + source + `
jobs:
- name: build
  plan:` + plan + `
`
	}

	for name, tc := range map[string]struct{ source, plan, want string }{
		"every key loads": {source: "    tag_filter: '^v(\\d+)[._](\\d+)[._](\\d+)$'\n    version_template: '$1.${2}.$3'\n    order_by: time\n    semver_constraint: '~3.4'\n    release: false\n    pre_release: true\n    drafts: true"},
		"named groups":    {source: "    tag_filter: '^v(?P<major>\\d+)_(?P<minor>\\d+)$'\n    version_template: '${major}.${minor}'"},
		"literal dollar":  {source: "    version_template: '$$$1'"},
		"misspelled key":  {source: "    tag_filer: x", want: `did you mean "tag_filter"`},
		"owner":           {source: "    owner: acme", want: "source.owner is Concourse's; steps names the repository in one key: repo: owner/name"},
		"access_token":    {source: "    access_token: abc", want: "source.access_token is Concourse's; name the environment variable"},
		"github_api_url":  {source: "    github_api_url: https://ghe.example/api/v3", want: "steps calls it endpoint"},
		"bad tag_filter":  {source: "    tag_filter: 'v(('", want: "source.tag_filter: error parsing regexp"},
		"group too high":  {source: "    tag_filter: '^v(\\d+)_(\\d+)$'\n    version_template: '$1.$3'", want: "$3 is not a group of tag_filter"},
		"group zero":      {source: "    version_template: '$0'", want: "$0 is not a group"},
		"unknown name":    {source: "    tag_filter: '^v(?P<major>\\d+)$'\n    version_template: '${minor}'", want: "${minor} names no group"},
		"glued name":      {source: "    version_template: '$1x'", want: "${1x} names no group"},
		"no group named":  {source: "    version_template: '1.0'", want: "names no group of tag_filter"},
		"bad order_by":    {source: "    order_by: date", want: `source.order_by: "date" is not an order (version, time)`},
		"bad constraint":  {source: "    semver_constraint: '~>> 1'", want: "source.semver_constraint"},
		"finds nothing":   {source: "    release: false", want: "source.release and source.pre_release are both false"},
		"get with params": {plan: "\n  - get: release\n    params: {globs: ['*.tgz']}", want: "takes no params"},
		"put to it":       {plan: "\n  - put: release", want: "only finds work"},
		"token as value":  {source: "    token_env: ghp_abc=123", want: "NAME of an environment variable"},
	} {
		_, err := LoadConfig(writeConfig(t, pipeline(tc.source, tc.plan)))

		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: err = %v, want it to load", name, err)
			}

			continue
		}

		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// TestGitHubReleaseVersionIsConcoursesGroup: with no template the version is the filter's LAST group, as Concourse reads it, or the whole match when there is none; a template joins groups; a tag the filter does not match has no version.
func TestGitHubReleaseVersionIsConcoursesGroup(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ filter, template, tag, want string }{
		"the default filter":        {"", "", "v24.22.3", "24.22.3"},
		"the default, no v":         {"", "", "1.2.0", "1.2.0"},
		"a v twice is no version":   {"", "", "vv1", ""},
		"the last of two groups":    {`^(api|web)-(.*)$`, "", "web-1.4.0", "1.4.0"},
		"no group: the whole match": {`\d+\.\d+`, "", "release-1.4-final", "1.4"},
		"an unmatched last group":   {`^v(\d+)(-rc)?`, "", "v3", ""},
		"a template joins groups":   {`^v(\d+)[._](\d+)[._](\d+)$`, "$1.$2.$3", "v3_4_13", "3.4.13"},
		"the template, dotted tag":  {`^v(\d+)[._](\d+)[._](\d+)$`, "$1.$2.$3", "v4.0.10", "4.0.10"},
		"no match":                  {`^v(\d+)[._](\d+)[._](\d+)$`, "$1.$2.$3", "v4.1.0-preview1", ""},
	} {
		source := map[string]any{"repo": "acme/app", "tag_filter": tc.filter, "version_template": tc.template}

		_, versioning, err := ParseGitHubReleaseSource(source)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if got := versioning.Version(tc.tag); got != tc.want {
			t.Errorf("%s: Version(%q) = %q, want %q", name, tc.tag, got, tc.want)
		}
	}
}

// TestGitHubReleaseConstraintAdmits: a constraint keeps the versions it allows and drops any that are not semver; with none every version passes.
func TestGitHubReleaseConstraintAdmits(t *testing.T) {
	t.Parallel()

	_, none, err := ParseGitHubReleaseSource(map[string]any{"repo": "acme/app"})
	if err != nil {
		t.Fatal(err)
	}

	_, line, err := ParseGitHubReleaseSource(map[string]any{"repo": "acme/app", "semver_constraint": "~24"})
	if err != nil {
		t.Fatal(err)
	}

	for version, want := range map[string]bool{"24.22.3": true, "24": true, "25.0.0": false, "22.23.5": false, "24.23.0-rc.1": false, "3_4_13": false, "": false} {
		if got := line.Admits(version); got != want {
			t.Errorf("~24 admits %q = %v, want %v", version, got, want)
		}
	}

	if !none.Admits("3_4_13") {
		t.Error("with no constraint a version that is not semver is refused, want it kept")
	}
}
