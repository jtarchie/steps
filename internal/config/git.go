package config

// type: git — the load-time rules for the built-in type's source:. A pipeline
// that declares its own `git` type replaces the built-in, and its source: is
// its own business.

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// GitSource is the built-in git type's source:. Anything else is refused: source: is free-form, so `fecth: true` would otherwise load and reproduce the silent staleness fetch: exists to prevent. Concourse's git resource ignores unknown keys; this is a deliberate divergence. validateGitSource checks it by hand rather than through decodeSource, to say why a quoted "false" is refused.
type GitSource struct {
	URI    string `yaml:"uri"`
	Branch string `yaml:"branch,omitempty"`
	Fetch  bool   `yaml:"fetch,omitempty"`
	// TagPattern is a glob (v*) that makes the resource track tags instead of a branch: one version per matching tag on the remote. Steps' own; concourse's tag_filter and tag_regex walk a branch's history instead.
	TagPattern string `yaml:"tag_pattern,omitempty"`
	// TokenEnv names the variable holding an https token. Setting it, or PrivateKeyEnv, makes every git command ignore the operator's global and system config: a resource handed a credential uses that credential and nothing else. Concourse's git resource is always that isolated, by running in a container; making it the default here would break every remote that today authenticates through the operator's own credential helper or ssh agent.
	TokenEnv string `yaml:"token_env,omitempty"`
	// PrivateKeyEnv names the variable holding an ssh private key; see TokenEnv.
	PrivateKeyEnv string `yaml:"private_key_env,omitempty"`
}

// GitPutParams is a put's params: on the built-in git type. The names are Concourse's git resource's, except force_with_lease, which is steps': a push made on behalf of a comment must fail if someone else moved the branch since.
type GitPutParams struct {
	Repository     string `yaml:"repository"`
	Branch         string `yaml:"branch,omitempty"`
	Force          bool   `yaml:"force,omitempty"`
	ForceWithLease string `yaml:"force_with_lease,omitempty"`
	// Ref is a revision inside Repository to push instead of its HEAD; steps' own, so a put can push the exact sha an earlier step verified.
	Ref string `yaml:"ref,omitempty"`
}

// gitType is the built-in git type.
const gitType = "git"

func (c *Config) validateGitResources() error {
	for _, resource := range c.Resources {
		if c.nativeType(resource.Type) != gitType {
			continue
		}

		err := validateGitSource(resource.Name, resource.Source)
		if err != nil {
			return err
		}

		err = validateGitCredential(resource)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateGitCredential refuses a credential the command could never see, and one that cannot authenticate the uri it is paired with.
func validateGitCredential(resource Resource) error {
	token, _ := resource.Source["token_env"].(string)
	key, _ := resource.Source["private_key_env"].(string)

	if token != "" && key != "" {
		return fmt.Errorf("resource %q: source.token_env and source.private_key_env are both set; a resource authenticates one way, so name one", resource.Name)
	}

	for field, name := range map[string]string{"token_env": token, "private_key_env": key} {
		err := checkGitCredentialVisible(resource, field, name)
		if err != nil {
			return err
		}
	}

	return checkGitCredentialURI(resource, token, key)
}

// checkGitCredentialURI pairs a credential with the scheme it can authenticate. fetch: talks to the clone's remote instead, which a token reaches in its https form.
func checkGitCredentialURI(resource Resource, token, key string) error {
	if fetch, _ := resource.Source["fetch"].(bool); fetch {
		return nil
	}

	uri, _ := resource.Source["uri"].(string)
	https := strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "http://")
	ssh := !https && (strings.Contains(uri, "://") || isSCPLike(uri))

	switch {
	case token != "" && !https:
		return fmt.Errorf("resource %q: source.token_env authenticates https, and %q is not an https uri", resource.Name, uri)
	case key != "" && !ssh:
		return fmt.Errorf("resource %q: source.private_key_env authenticates ssh, and %q is not an ssh uri", resource.Name, uri)
	}

	return nil
}

func checkGitCredentialVisible(resource Resource, field, name string) error {
	if name == "" {
		return nil
	}

	if !envVarPattern.MatchString(name) {
		return fmt.Errorf("resource %q: source.%s %q is not an environment variable name", resource.Name, field, name)
	}

	_, secret := resource.Secrets[name]
	if !secret && !slices.Contains(resource.Env, name) {
		return fmt.Errorf("resource %q: source.%s names %s, which the resource's commands cannot see; list it in the resource's env or secrets", resource.Name, field, name)
	}

	return nil
}

// validateGitPuts checks each put to a built-in git resource's params:.
func (c *Config) validateGitPuts() error {
	for i := range c.Jobs {
		err := c.Jobs[i].visitSteps(func(label string, step *Step) error {
			name, ok := step.resourceName()
			if !ok || step.Put == "" {
				return nil
			}

			resource, err := c.FindResource(name)
			if err != nil || c.nativeType(resource.Type) != gitType {
				return nil //nolint:nilerr // an unknown resource is reported by the rule that owns it
			}

			err = checkGitPut(step, resource)
			if err != nil {
				return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func checkGitPut(step *Step, resource *Resource) error {
	var params GitPutParams

	err := decodeParams(step.Params, &params)
	if err != nil {
		return err
	}

	err = checkGitPutParams(step, params)
	if err != nil {
		return err
	}

	branch := params.Branch
	if branch == "" {
		branch, _ = resource.Source["branch"].(string)
	}

	if tags, _ := resource.Source["tag_pattern"].(string); tags != "" {
		return errors.New("the resource has source.tag_pattern, which makes it read-only: tags are listed and checked out, never pushed")
	}

	switch {
	case branch == "":
		return errors.New("params.branch is required when the resource has no source.branch: the branch to push to")
	case !isPlainBranchName(branch) || strings.HasPrefix(branch, "refs/"):
		return fmt.Errorf("branch %q is not a plain branch name", branch)
	}

	return nil
}

func checkGitPutParams(step *Step, params GitPutParams) error {
	switch root := firstPathComponent(params.Repository); {
	case params.Repository == "":
		return errors.New("params.repository is required: the input holding the commits to push")
	case !step.InputsAll() && !slices.Contains(step.InputNames(), root):
		return fmt.Errorf("params.repository %q names artifact %q, which the put does not list in its inputs", params.Repository, root)
	case params.Force && params.ForceWithLease != "":
		return errors.New("params.force and params.force_with_lease are both set; a lease exists to make a force push conditional, so set one")
	case strings.HasPrefix(params.Ref, "-"):
		return fmt.Errorf("params.ref %q starts with a dash, which git would read as an option", params.Ref)
	case params.ForceWithLease != "" && !strings.Contains(params.ForceWithLease, "((") && !isHexSHA(params.ForceWithLease):
		return fmt.Errorf("params.force_with_lease %q is not a commit sha", params.ForceWithLease)
	}

	return nil
}

func isHexSHA(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}

	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}

	return true
}

func validateGitSource(name string, source map[string]any) error {
	allowed := yamlKeys(GitSource{})

	for _, key := range slices.Sorted(maps.Keys(source)) {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("resource %q: the git type has no source.%s%s (it reads %s)",
				name, key, suggestion(key, allowed), strings.Join(allowed, ", "))
		}
	}

	err := validateGitTagPattern(name, source)
	if err != nil {
		return err
	}

	raw, present := source["fetch"]
	if !present {
		return nil
	}

	fetch, isBool := raw.(bool)
	if !isBool {
		return fmt.Errorf("resource %q: source.fetch must be true or false, not %v — a quoted \"false\" would read as true", name, raw)
	}

	if !fetch {
		return nil
	}

	uri, _ := source["uri"].(string)
	branch, _ := source["branch"].(string)

	return validateGitFetch(name, uri, branch)
}

func validateGitTagPattern(name string, source map[string]any) error {
	raw, present := source["tag_pattern"]
	if !present {
		return nil
	}

	pattern, isString := raw.(string)
	fetch, _ := source["fetch"].(bool)
	branch, _ := source["branch"].(string)

	switch {
	case !isString || pattern == "":
		return fmt.Errorf("resource %q: source.tag_pattern must be a string glob such as \"v*\", not %v", name, raw)
	case strings.HasPrefix(pattern, "-") || strings.ContainsFunc(pattern, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return fmt.Errorf("resource %q: source.tag_pattern %q has a leading dash, a space or a control character, which no tag name has", name, pattern)
	case branch != "":
		return fmt.Errorf("resource %q: source.tag_pattern and source.branch are both set; a resource tracks tags or one branch, so name one", name)
	case fetch:
		return fmt.Errorf("resource %q: source.fetch refreshes a clone's branch, and tag_pattern lists the tags of the uri itself, so the two cannot combine", name)
	}

	return nil
}

func validateGitFetch(name, uri, branch string) error {
	switch {
	case filepath.IsAbs(uri):
	case strings.Contains(uri, "://") || isSCPLike(uri):
		return fmt.Errorf("resource %q: source.fetch refreshes a LOCAL clone; %q is remote, and a remote uri is already fetched fresh by every check", name, uri)
	default:
		return fmt.Errorf("resource %q: source.fetch needs source.uri to be an absolute path, not %q — write the absolute path; it is resolved on the machine that runs the check", name, uri)
	}

	if branch == "" {
		return fmt.Errorf("resource %q: source.fetch needs source.branch — a clone's refs/remotes/<remote>/HEAD is set when it is cloned and no fetch ever moves it", name)
	}

	if strings.HasPrefix(branch, "refs/") {
		return fmt.Errorf("resource %q: source.branch %q is a ref; with source.fetch, name the branch (e.g. main), not the ref", name, branch)
	}

	if !isPlainBranchName(branch) {
		return fmt.Errorf("resource %q: source.branch %q is not a valid branch name, and with source.fetch it becomes part of a refspec", name, branch)
	}

	return nil
}

// isSCPLike reports git's scp-style `host:path`: a colon before any slash.
func isSCPLike(uri string) bool {
	colon := strings.Index(uri, ":")

	return colon > 0 && !strings.Contains(uri[:colon], "/")
}

// isPlainBranchName applies git check-ref-format's rules to a branch name,
// plus a leading `-`, which git fetch would read as an option.
func isPlainBranchName(branch string) bool {
	switch {
	case branch == "@",
		strings.HasPrefix(branch, "-"),
		strings.HasPrefix(branch, "/"),
		strings.HasSuffix(branch, "/"),
		strings.HasSuffix(branch, "."),
		strings.Contains(branch, "//"),
		strings.Contains(branch, ".."),
		strings.Contains(branch, "@{"),
		strings.ContainsAny(branch, " ~^:?*[\\\x7f"):
		return false
	}

	for _, r := range branch {
		if r < ' ' {
			return false
		}
	}

	for component := range strings.SplitSeq(branch, "/") {
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}

	return true
}
