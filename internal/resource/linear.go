package resource

// The linear-* built-ins' check, in and out (see config/linear.go for what each type is and why they are split).

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
)

// linearStages is what one linear-* type implements: a finder has a check and an in, a publisher an out, and an in when its put leaves an issue a get after it reads back.
type linearStages struct {
	check func(ctx context.Context, source map[string]any) ([]map[string]any, error)
	in    func(ctx context.Context, source, version map[string]any, destDir string) error
	out   func(ctx context.Context, source, params map[string]any, inputs PutInputs, srcDir string) (map[string]any, error)
}

var linearTypes = map[string]linearStages{ //nolint:gochecknoglobals // read-only dispatch table, as githubTypes
	config.LinearIssuesType:  {check: linearIssuesCheck, in: linearIssuesIn},
	config.LinearCommentType: {out: linearCommentOut},
	config.LinearUpdateType:  {out: linearUpdateOut},
	config.LinearIssueType:   {in: linearIssueIn, out: linearIssueOut},
}

type linearBackend struct{}

func (linearBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	rt := c.Type

	stage := linearTypes[rt.Config.Linear].check
	if stage == nil {
		// A put-only type has nothing to find, and the load rule refuses a get of one before this is reached.
		return nil, nil
	}

	versions, err := stage(ctx, c.Resource.Source)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

func (linearBackend) in(ctx context.Context, c Call) error {
	rt := c.Type

	stage := linearTypes[rt.Config.Linear].in
	if stage == nil {
		return fmt.Errorf("in %q: a %s resource only publishes, so there is nothing to fetch", rt.Name, rt.Config.Linear)
	}

	err := stage(ctx, c.Resource.Source, c.Version, c.Dir)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.fetched", "resource_type", rt.Name, "dest_dir", c.Dir)

	return nil
}

func (linearBackend) out(ctx context.Context, c Call) (map[string]any, error) {
	rt := c.Type

	stage := linearTypes[rt.Config.Linear].out
	if stage == nil {
		return nil, fmt.Errorf("out %q: a %s resource only finds work, so there is nothing to publish to", rt.Name, rt.Config.Linear)
	}

	version, err := stage(ctx, c.Resource.Source, c.Params, c.Inputs, c.Dir)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.put", "resource_type", rt.Name, "src_dir", c.Dir, "result", version)

	return version, nil
}
