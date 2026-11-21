package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"

	"github.com/Masterminds/semver/v3"
)

// GitHubReleaseType finds a repository's releases, as Concourse's github-release resource does; it does not publish them (see github.go for why find and publish are separate types).
const GitHubReleaseType = "github-release"

// The orders a github-release check can sort by, as Concourse spells them.
const (
	GitHubReleaseByVersion = "version"
	GitHubReleaseByTime    = "time"
)

// defaultReleaseTagFilter is Concourse's: an optional leading v, then the version. Its code anchors it though its README does not, and the code is what pipelines run against.
const defaultReleaseTagFilter = `^v?([^v].*)`

// GitHubReleaseSource is a github-release resource's source:. The filters and their defaults are Concourse's; the connection is the github-* family's.
type GitHubReleaseSource struct {
	Repo     string `yaml:"repo"`
	TokenEnv string `yaml:"token_env,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
	// TagFilter is an RE2 pattern a release's tag must match. Its last capture group is the release's version, or the whole match when it has none, as in Concourse.
	TagFilter string `yaml:"tag_filter,omitempty"`
	// VersionTemplate builds the version from TagFilter's groups instead ($1, ${name}, as Go's Regexp.Expand reads them), for a project whose tags spell a version in pieces: v3_4_13. Steps' own; Concourse has no way to join groups.
	VersionTemplate string `yaml:"version_template,omitempty"`
	// OrderBy is version (the default), the version read from the tag, or time, when the release was published.
	OrderBy string `yaml:"order_by,omitempty"`
	// SemverConstraint keeps only the releases whose version satisfies it, such as ~24 or ">= 1.2, < 3".
	SemverConstraint string `yaml:"semver_constraint,omitempty"`
	// Release finds final releases; true when unset, as in Concourse.
	Release *bool `yaml:"release,omitempty"`
	// PreRelease finds pre-releases too.
	PreRelease bool `yaml:"pre_release,omitempty"`
	// Drafts finds drafts and nothing else.
	Drafts bool `yaml:"drafts,omitempty"`
}

// Connection is where this resource talks to.
func (s GitHubReleaseSource) Connection() GitHubConnection {
	return GitHubConnection{Repo: s.Repo, TokenEnv: s.TokenEnv, Endpoint: s.Endpoint}
}

// FindsReleases is source.release with its default filled in.
func (s GitHubReleaseSource) FindsReleases() bool { return s.Release == nil || *s.Release }

// GitHubReleaseVersioning is how a github-release source reads a version from a tag and which versions it keeps, compiled once at parse.
type GitHubReleaseVersioning struct {
	pattern    *regexp.Regexp
	template   string
	constraint *semver.Constraints
}

// Matches reports whether tag passes the tag filter at all.
func (v GitHubReleaseVersioning) Matches(tag string) bool { return v.pattern.MatchString(tag) }

// Version is the version tag spells: the template expanded over the match, else the filter's last group, else the whole match; "" when the tag does not match. The last group, not the first, because that is what Concourse reads.
func (v GitHubReleaseVersioning) Version(tag string) string {
	match := v.pattern.FindStringSubmatchIndex(tag)
	if match == nil {
		return ""
	}

	if v.template != "" {
		return string(v.pattern.ExpandString(nil, v.template, tag, match))
	}

	last := len(match) - 2
	if match[last] < 0 {
		return ""
	}

	return tag[match[last]:match[last+1]]
}

// Admits reports whether a version passes semver_constraint; every version does when there is none, and none that is not semver does when there is one.
func (v GitHubReleaseVersioning) Admits(version string) bool {
	if v.constraint == nil {
		return true
	}

	parsed, err := semver.NewVersion(version)

	return err == nil && v.constraint.Check(parsed)
}

// concourseReleaseKeys are Concourse's github-release source keys steps spells differently or leaves out, each with what to write instead: a ported pipeline's first error should say what to do, not list the valid keys.
var concourseReleaseKeys = map[string]string{ //nolint:gochecknoglobals,gosec // read-only table of key names and hints; no credential in it
	"owner":              "steps names the repository in one key: repo: owner/name",
	"repository":         "steps names the repository in one key: repo: owner/name",
	"user":               "steps names the repository in one key: repo: owner/name",
	"access_token":       "name the environment variable that holds the token in token_env (GH_TOKEN when unset), or map it from secrets:; a literal token would be hashed into the state database",
	"github_api_url":     "steps calls it endpoint, as every github-* type does",
	"github_v4_api_url":  "steps reads releases over REST alone, so there is no GraphQL URL to set; endpoint is the REST base",
	"github_uploads_url": "github-release only finds releases, so there is nothing to upload",
	"insecure":           "steps does not skip TLS verification; an http endpoint is accepted for a loopback host only",
	"app_id":             "the github-* types authenticate with a token alone; mint an installation token into the variable token_env names",
	"private_key":        "the github-* types authenticate with a token alone; mint an installation token into the variable token_env names",
	"asset_dir":          "a github-release get downloads no assets yet",
}

// ParseGitHubReleaseSource decodes a github-release source: strictly, checks it, and compiles what it reads tags with, so a bad pattern or constraint is a load error.
func ParseGitHubReleaseSource(raw map[string]any) (GitHubReleaseSource, GitHubReleaseVersioning, error) {
	var source GitHubReleaseSource

	err := refuseConcourseReleaseKeys(raw)
	if err != nil {
		return source, GitHubReleaseVersioning{}, err
	}

	err = decodeSource(raw, &source)
	if err != nil {
		return source, GitHubReleaseVersioning{}, err
	}

	err = validateGitHubConnection(source.Connection())
	if err != nil {
		return source, GitHubReleaseVersioning{}, err
	}

	versioning, err := source.versioning()
	if err != nil {
		return source, GitHubReleaseVersioning{}, err
	}

	return source, versioning, source.validateSelection()
}

func refuseConcourseReleaseKeys(raw map[string]any) error {
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		if hint, ok := concourseReleaseKeys[key]; ok {
			return fmt.Errorf("source.%s is Concourse's; %s", key, hint)
		}
	}

	return nil
}

func (s GitHubReleaseSource) validateSelection() error {
	switch {
	case s.OrderBy != "" && s.OrderBy != GitHubReleaseByVersion && s.OrderBy != GitHubReleaseByTime:
		return fmt.Errorf("source.order_by: %q is not an order (%s, %s)", s.OrderBy, GitHubReleaseByVersion, GitHubReleaseByTime)
	// Concourse's check keeps every release under this pair, finals and pre-releases alike, which no one writing both falses means.
	case !s.FindsReleases() && !s.PreRelease:
		return errors.New("source.release and source.pre_release are both false, which finds no release; set pre_release: true to follow pre-releases alone")
	}

	return nil
}

func (s GitHubReleaseSource) versioning() (GitHubReleaseVersioning, error) {
	filter := s.TagFilter
	if filter == "" {
		filter = defaultReleaseTagFilter
	}

	pattern, err := regexp.Compile(filter)
	if err != nil {
		return GitHubReleaseVersioning{}, fmt.Errorf("source.tag_filter: %w", err)
	}

	err = validateVersionTemplate(pattern, s.VersionTemplate)
	if err != nil {
		return GitHubReleaseVersioning{}, err
	}

	versioning := GitHubReleaseVersioning{pattern: pattern, template: s.VersionTemplate}

	if s.SemverConstraint != "" {
		versioning.constraint, err = semver.NewConstraint(s.SemverConstraint)
		if err != nil {
			return GitHubReleaseVersioning{}, fmt.Errorf("source.semver_constraint: %q: %w", s.SemverConstraint, err)
		}
	}

	return versioning, nil
}

// templateReference is one $name, ${name} or $1 in a template, as Regexp.Expand reads them; $$ is a literal dollar.
var templateReference = regexp.MustCompile(`\$(\$|\{([^}]*)\}|([A-Za-z0-9_]+))`) //nolint:gochecknoglobals // compiled once, read-only

// validateVersionTemplate refuses a reference to a group the filter does not have: Expand would quietly put nothing there, and every tag would read as the same version.
func validateVersionTemplate(pattern *regexp.Regexp, template string) error {
	if template == "" {
		return nil
	}

	references := templateReference.FindAllStringSubmatch(template, -1)
	if len(references) == 0 {
		return fmt.Errorf("source.version_template: %q names no group of tag_filter, so every tag would read as the same version; use $1, ${2} or ${name}", template)
	}

	for _, reference := range references {
		name := reference[2] + reference[3]
		if reference[1] == "$" {
			continue
		}

		number, err := strconv.Atoi(name)
		if err == nil {
			if number < 1 || number > pattern.NumSubexp() {
				return fmt.Errorf("source.version_template: $%s is not a group of tag_filter %s, which has %d", name, pattern.String(), pattern.NumSubexp())
			}

			continue
		}

		if pattern.SubexpIndex(name) < 0 {
			return fmt.Errorf("source.version_template: ${%s} names no group of tag_filter %s", name, pattern.String())
		}
	}

	return nil
}
