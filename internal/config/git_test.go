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

func gitCredentialPipeline(resource, put string) string {
	return `
resources:
- name: repo
  type: git
` + resource + `
jobs:
- name: j
  plan:
  - get: repo
` + put
}

func TestGitCredentialRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ resource, put, want string }{
		"token unseen": {`  source: { uri: "https://h/x.git", token_env: T }`, "", "cannot see; list it in the resource's env or secrets"},
		"key unseen":   {`  source: { uri: "git@h:x", private_key_env: K }`, "", "cannot see"},
		"both":         {"  env: [T, K]\n  source: { uri: \"https://h/x.git\", token_env: T, private_key_env: K }", "", "both set"},
		"bad name":     {"  env: [T]\n  source: { uri: \"https://h/x.git\", token_env: \"T-1\" }", "", "not an environment variable name"},
		"token ssh":    {"  env: [T]\n  source: { uri: \"git@h:x\", token_env: T }", "", "not an https uri"},
		"key https":    {"  env: [K]\n  source: { uri: \"https://h/x.git\", private_key_env: K }", "", "not an ssh uri"},
		"key local":    {"  env: [K]\n  source: { uri: /x, private_key_env: K }", "", "not an ssh uri"},
		"no repository": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - put: repo\n    inputs: [repo]\n    params: { branch: b }", "params.repository is required"},
		"undeclared repository": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - put: repo\n    inputs: [other]\n    params: { repository: repo }", "does not list in its inputs"},
		"no branch": {"  source: { uri: \"https://h/x.git\" }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo }", "params.branch is required"},
		"bad branch": {"  source: { uri: \"https://h/x.git\" }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo, branch: \"a..b\" }", "not a plain branch name"},
		"force and lease": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo, force: true, force_with_lease: abc123 }", "both set"},
		"lease not a sha": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo, force_with_lease: main }", "not a commit sha"},
		"dash ref": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo, ref: \"--upload-pack=x\" }", "starts with a dash"},
		"unknown param": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo, rebase: true }", "rebase"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, gitCredentialPipeline(tc.resource, tc.put)))
			if err == nil {
				t.Fatalf("loaded, want an error containing %q", tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestGitCredentialLoads(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ resource, put string }{
		"token in env":     {"  env: [T]\n  source: { uri: \"https://h/x.git\", token_env: T }", ""},
		"token in secrets": {"  secrets: { T: \"((ci:t))\" }\n  source: { uri: \"https://h/x.git\", token_env: T }", ""},
		"key scp":          {"  env: [K]\n  source: { uri: \"git@h:x\", private_key_env: K }", ""},
		"token fetch":      {"  env: [T]\n  source: { uri: /x, branch: main, fetch: true, token_env: T }", ""},
		"put": {"  env: [T]\n  source: { uri: \"https://h/x.git\", branch: main, token_env: T }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo/sub, force_with_lease: 0123abcd }"},
		"lease from load_var": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - task: t\n    outputs: [m]\n    run: echo abc > m/sha\n  - load_var: lease\n    inputs: [m]\n    file: m/sha\n  - put: repo\n    inputs: [repo]\n    params: { repository: repo, force_with_lease: ((lease)) }"},
		"put ref": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo, ref: 0123abcd, force_with_lease: 4567 }"},
		"put ref from load_var": {"  source: { uri: \"https://h/x.git\", branch: main }",
			"  - task: t\n    outputs: [m]\n    run: echo abc > m/sha\n  - load_var: sha\n    inputs: [m]\n    file: m/sha\n  - put: repo\n    inputs: [repo]\n    params: { repository: repo, ref: ((sha)) }"},
		"put branch param": {"  source: { uri: \"https://h/x.git\" }",
			"  - put: repo\n    inputs: [repo]\n    params: { repository: repo, branch: feature/x, force: true }"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, gitCredentialPipeline(tc.resource, tc.put)))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
		})
	}
}
