package config

import (
	"strings"
	"testing"
)

func gitPipeline(source string) string {
	return `
resources:
- name: repo
  type: git
  source: ` + source + `
jobs:
- name: j
  plan: [{ get: repo }]
`
}

func TestGitSourceRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ source, want string }{
		"https":             {`{ uri: "https://github.com/x/y.git", branch: main, fetch: true }`, "a remote uri is already fetched fresh"},
		"scp":               {`{ uri: "git@host:x/y", branch: main, fetch: true }`, "a remote uri is already fetched fresh"},
		"file url":          {`{ uri: "file:///x", branch: main, fetch: true }`, "a remote uri is already fetched fresh"},
		"relative":          {`{ uri: ./x, branch: main, fetch: true }`, "write the absolute path"},
		"tilde":             {`{ uri: ~/x, branch: main, fetch: true }`, "write the absolute path"},
		"leading dash":      {`{ uri: -x, branch: main, fetch: true }`, "write the absolute path"},
		"missing uri":       {`{ branch: main, fetch: true }`, "write the absolute path"},
		"string fetch":      {`{ uri: /x, branch: main, fetch: "yes" }`, "source.fetch must be true or false"},
		"no branch":         {`{ uri: /x, fetch: true }`, "source.fetch needs source.branch"},
		"ref branch":        {`{ uri: /x, branch: refs/heads/main, fetch: true }`, "name the branch (e.g. main), not the ref"},
		"colon branch":      {`{ uri: /x, branch: "a:b", fetch: true }`, "not a valid branch name"},
		"glob branch":       {`{ uri: /x, branch: "*", fetch: true }`, "not a valid branch name"},
		"dash branch":       {`{ uri: /x, branch: -x, fetch: true }`, "not a valid branch name"},
		"dotdot branch":     {`{ uri: /x, branch: a..b, fetch: true }`, "not a valid branch name"},
		"lock branch":       {`{ uri: /x, branch: a/b.lock, fetch: true }`, "not a valid branch name"},
		"dot component":     {`{ uri: /x, branch: a/.b, fetch: true }`, "not a valid branch name"},
		"trailing slash":    {`{ uri: /x, branch: a/, fetch: true }`, "not a valid branch name"},
		"control character": {`{ uri: /x, branch: "a\tb", fetch: true }`, "not a valid branch name"},
		"typo":              {`{ uri: /x, branch: main, fecth: true }`, `did you mean "fetch"?`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, gitPipeline(tc.source)))
			if err == nil {
				t.Fatalf("source %s loaded, want an error containing %q", tc.source, tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `resource "repo"`) {
				t.Errorf("error = %v, want it to name resource \"repo\" and contain %q", err, tc.want)
			}
		})
	}
}

func TestGitSourceLoads(t *testing.T) {
	t.Parallel()

	for name, pipeline := range map[string]string{
		"local fetch":      gitPipeline(`{ uri: /Users/me/repo, branch: feature/x-1, fetch: true }`),
		"fetch off remote": gitPipeline(`{ uri: "https://github.com/x/y.git", fetch: false }`),
		"no fetch":         gitPipeline(`{ uri: "https://github.com/x/y.git", branch: main }`),
		// A user's own git type replaces the built-in, and so do its rules.
		"user git type": `
resource_types:
- name: git
  config:
    check: echo '[]'
    in: "true"
resources:
- name: repo
  type: git
  source: { uri: "https://github.com/x/y.git", fetch: true, private_key: k }
jobs:
- name: j
  plan: [{ get: repo }]
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, pipeline))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
		})
	}
}
