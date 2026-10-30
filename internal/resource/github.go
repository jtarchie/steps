package resource

// The github-* built-ins' check, in and out (see config/github.go for what
// each type is and why they are split).

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

const (
	// githubPageSize is GitHub's largest page.
	githubPageSize = 100
	// githubPageLimit bounds one check. A search answers at most a thousand
	// results anyway; a comment listing past a cursor this far behind means
	// more than a thousand comments arrived between two polls, and the
	// newest thousand are the ones worth reading.
	githubPageLimit = 10
)

// The files a pull request's metadata is written to, beside its tree.
const (
	prJSONFile      = "pr.json"
	prDiffFile      = "pr.diff"
	prNumberFile    = "pr.number"
	prSHAFile       = "pr.sha"
	prMergeBaseFile = "pr.mergebase"
	prURLFile       = "pr.url"
)

// githubStages is what one github-* type implements: a finder has a check
// and an in, a publisher only an out. A nil stage is one the type does not
// have.
type githubStages struct {
	check func(ctx context.Context, source, cursor map[string]any) ([]map[string]any, error)
	in    func(ctx context.Context, source, version map[string]any, destDir string) error
	out   func(ctx context.Context, source, params map[string]any, inputs PutInputs, srcDir string) (map[string]any, error)
}

// githubTypes is the one dispatch over the github-* types.
var githubTypes = map[string]githubStages{
	config.GitHubPRsType: {
		check: func(ctx context.Context, source, _ map[string]any) ([]map[string]any, error) {
			return githubPRsCheck(ctx, source)
		},
		in: githubPRsIn,
	},
	config.GitHubCommentsType:  {check: githubCommentsCheck, in: githubCommentsIn},
	config.GitHubPRCommentType: {out: githubCommentOut},
	config.GitHubPRReviewType:  {out: githubReviewOut},
	config.GitHubReactionType:  {out: githubReactionOut},
	config.GitHubLabelType:     {out: githubLabelOut},
	config.GitHubIssueType:     {out: githubIssueOut},
}

type githubBackend struct{}

func (githubBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	rt := c.Type

	stage := githubTypes[rt.Config.GitHub].check
	if stage == nil {
		// No check: a put-only type has nothing to find, and the load rule
		// refuses a get of one before this is reached.
		return nil, nil
	}

	versions, err := stage(ctx, c.Resource.Source, c.Version)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

func (githubBackend) in(ctx context.Context, c Call) error {
	rt := c.Type

	stage := githubTypes[rt.Config.GitHub].in
	if stage == nil {
		return fmt.Errorf("in %q: a %s resource only publishes, so there is nothing to fetch", rt.Name, rt.Config.GitHub)
	}

	err := stage(ctx, c.Resource.Source, c.Version, c.Dir)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.fetched", "resource_type", rt.Name, "dest_dir", c.Dir)

	return nil
}

func (githubBackend) out(ctx context.Context, c Call) (map[string]any, error) {
	rt := c.Type

	stage := githubTypes[rt.Config.GitHub].out
	if stage == nil {
		return nil, fmt.Errorf("out %q: a %s resource only finds work, so there is nothing to publish to", rt.Name, rt.Config.GitHub)
	}

	version, err := stage(ctx, c.Resource.Source, c.Params, c.Inputs, c.Dir)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.put", "resource_type", rt.Name, "src_dir", c.Dir, "result", version)

	return version, nil
}

// versionField reads one field of a version as the string it was minted
// as. A version decoded from the store may carry a json.Number.
func versionField(version map[string]any, key string) string {
	value, ok := version[key]
	if !ok || value == nil {
		return ""
	}

	return fmt.Sprint(value)
}

// versionNumber is versionField for an id or number GitHub minted, "" unless it is decimal: it is spliced into an API path, so `..` or a slash would steer the request.
func versionNumber(version map[string]any, key string) string {
	value := versionField(version, key)
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return ""
	}

	return value
}
