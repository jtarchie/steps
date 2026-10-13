package resource

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
)

// Call is one stage of one resource. CheckVersions, RunIn and RunOut each
// read only the fields their stage has.
type Call struct {
	// Config resolves an mcp-backed type's mcp_servers: entry; no other
	// backend reads it, so nil is fine whenever the type is not mcp-backed.
	Config *config.Config
	Type   config.ResourceType
	// Resource supplies source: and the instance's own env: (see
	// config.Resource.Env), unioned with Type's for this call only.
	Resource config.Resource
	// Version is a check's cursor or the version an in: fetches.
	Version map[string]any
	// Params is a get's or a put's params:.
	Params map[string]any
	// Inputs is what a put's out: may read of its build; only expr and
	// github read it.
	Inputs PutInputs
	// Dir is an in:'s destination or an out:'s source tree.
	Dir string
}

// backend is one way a resource type implements its three stages.
type backend interface {
	check(ctx context.Context, c Call) ([]map[string]any, error)
	in(ctx context.Context, c Call) error
	out(ctx context.Context, c Call) (map[string]any, error)
}

// backendFor is the one dispatch over config.ResourceBackend; `exhaustive`
// fails the build here when a backend is added and not answered for.
func backendFor(rt config.ResourceType) backend {
	switch rt.Config.Backend() {
	case config.BackendMCP:
		return mcpBackend{}
	case config.BackendExpr:
		return exprBackend{}
	case config.BackendWebhook:
		return webhookBackend{}
	case config.BackendCron:
		return cronBackend{}
	case config.BackendGitHub:
		return githubBackend{}
	case config.BackendShell:
	}

	return shellBackend{}
}

type mcpBackend struct{}

func (mcpBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	return mcpCheckVersions(ctx, c.Config, c.Type, c.Resource.Source, c.Version)
}

func (mcpBackend) in(ctx context.Context, c Call) error {
	return mcpRunIn(ctx, c.Config, c.Type, c.Resource.Source, c.Version, c.Params, c.Dir)
}

func (mcpBackend) out(ctx context.Context, c Call) (map[string]any, error) {
	return mcpRunOut(ctx, c.Config, c.Type, c.Resource.Source, c.Params, c.Dir)
}

type exprBackend struct{}

func (exprBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	return exprCheckVersions(ctx, c.Type, c.Resource.Env, c.Resource.Source, c.Version)
}

func (exprBackend) in(ctx context.Context, c Call) error {
	return exprRunIn(ctx, c.Type, c.Resource.Env, c.Resource.Source, c.Version, c.Params, c.Dir)
}

func (exprBackend) out(ctx context.Context, c Call) (map[string]any, error) {
	return exprRunOut(ctx, c.Type, c.Resource.Env, c.Resource.Source, c.Params, c.Inputs, c.Dir)
}

type webhookBackend struct{}

// check finds nothing: a webhook resource's versions are the deliveries it
// was sent, and a get with none recorded fails as "no versions available",
// which is the truth.
func (webhookBackend) check(context.Context, Call) ([]map[string]any, error) {
	return nil, nil
}

func (webhookBackend) in(context.Context, Call) error {
	return errWebhookIn
}

func (webhookBackend) out(_ context.Context, c Call) (map[string]any, error) {
	return nil, fmt.Errorf("out %q: a webhook resource cannot be put to", c.Type.Name)
}

type cronBackend struct{}

func (cronBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	return cronCheckVersions(ctx, c.Type, c.Resource.Source, c.Version)
}

func (cronBackend) in(_ context.Context, c Call) error {
	return cronRunIn(c.Type, c.Resource.Source, c.Version, c.Dir)
}

func (cronBackend) out(_ context.Context, c Call) (map[string]any, error) {
	return nil, fmt.Errorf("out %q: %w", c.Type.Name, errCronOut)
}
