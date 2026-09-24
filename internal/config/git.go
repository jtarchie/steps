package config

// type: git — the load-time rules for the built-in type's source:. A pipeline
// that declares its own `git` type replaces the built-in, and its source: is
// its own business.

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// gitSourceKeys is every source: key the built-in git type reads. Anything
// else is refused: source: is free-form, so `fecth: true` would otherwise load
// and reproduce the silent staleness fetch: exists to prevent. Concourse's git
// resource ignores unknown keys; this is a deliberate divergence.
var gitSourceKeys = []string{"uri", "branch", "fetch"} //nolint:gochecknoglobals // read-only table

func (c *Config) validateGitResources() error {
	builtin, err := ReadBuiltinResourceType("git")
	if err != nil {
		return err
	}

	for _, resource := range c.Resources {
		index := c.findResourceTypeIndex(resource.Type)
		// Content identity: a user type named git runs its own commands, so only the built-in's check: gets the built-in's rules.
		if index < 0 || c.ResourceTypes[index].Config.Check != builtin.Config.Check {
			continue
		}

		err := validateGitSource(resource.Name, resource.Source)
		if err != nil {
			return err
		}
	}

	return nil
}

func validateGitSource(name string, source map[string]any) error {
	keys := slices.Sorted(maps.Keys(source))

	for _, key := range keys {
		if !slices.Contains(gitSourceKeys, key) {
			return fmt.Errorf("resource %q: the git type has no source.%s%s (it reads %s)",
				name, key, suggestion(key, gitSourceKeys), strings.Join(gitSourceKeys, ", "))
		}
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
