package config

// The built-in linear-* resource types: their names, the shape of each one's source: and put params:, and the load-time rules that need no network.

// Split as the github-* types are, and for the same reason: linear-issues FINDS work (check and in, no out), linear-comment and linear-update PUBLISH (out, no check). A put records its version in the resource's own history, so a type that both watched and posted would mint a comment-shaped version into the history it fans out over, and under trigger: true a post would trigger the build that posts again.

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The built-in Linear type names.
const (
	LinearIssuesType  = "linear-issues"
	LinearCommentType = "linear-comment"
	LinearUpdateType  = "linear-update"
)

// LinearTypes is every linear-* type, in the order the docs list them.
func LinearTypes() []string {
	return []string{LinearIssuesType, LinearCommentType, LinearUpdateType}
}

// LinearFinds reports whether kind is the type a get reads.
func LinearFinds(kind string) bool { return kind == LinearIssuesType }

// LinearFetches reports whether a linear-* type has an in:, which a get after
// its put fetches with. Only the finder does today.
func LinearFetches(kind string) bool { return LinearFinds(kind) }

// DefaultLinearTokenEnv is the variable a linear-* resource reads its key from when source.token_env does not name another.
const DefaultLinearTokenEnv = "LINEAR_API_KEY" //nolint:gosec // the variable's name, not a credential

// DefaultLinearEndpoint is Linear's GraphQL API.
const DefaultLinearEndpoint = "https://api.linear.app/graphql"

// LinearAssigneeMe is the assignee that means the key's own user.
const LinearAssigneeMe = "@me"

// LinearStateTypes are Linear's fixed workflow-state categories, which every team's custom states belong to one of.
var LinearStateTypes = []string{"triage", "backlog", "unstarted", "started", "completed", "canceled"} //nolint:gochecknoglobals // read-only table

// linearIdentifierPattern is an issue's identifier: its team key, a dash, its number.
var linearIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9]+-[0-9]+$`)

// linearIDPattern is what a Linear id or a user id looks like: a uuid, which is what @me stands in for.
var linearIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// LinearConnection is where a linear-* resource talks to and as whom, common to every source: shape.
type LinearConnection struct {
	TokenEnv string
	Endpoint string
}

// Token is the variable name the resource reads.
func (c LinearConnection) Token() string {
	if c.TokenEnv == "" {
		return DefaultLinearTokenEnv
	}

	return c.TokenEnv
}

// API is the GraphQL endpoint.
func (c LinearConnection) API() string {
	if c.Endpoint == "" {
		return DefaultLinearEndpoint
	}

	return c.Endpoint
}

// LinearIssuesSource is a linear-issues resource's source:. Every filter narrows: an issue is a version only while it matches all of them.
type LinearIssuesSource struct {
	// Team is a team key (ENG) or name. Required: state and label names are per team in Linear, and an unscoped state name matches every team's.
	Team string `yaml:"team"`
	// States are state names in Team, any of which matches.
	States []string `yaml:"states,omitempty"`
	// StateTypes are Linear's fixed categories, for a pipeline that does not want to name a team's custom states. Not with States.
	StateTypes []string `yaml:"state_types,omitempty"`
	// Labels must all be on the issue, team or workspace labels.
	Labels []string `yaml:"labels,omitempty"`
	// Project is a project name or id.
	Project string `yaml:"project,omitempty"`
	// Assignee is @me, the key's own user, or a user id. Never an email: that puts a person identifier in pipeline YAML.
	Assignee string `yaml:"assignee,omitempty"`
	// TokenEnv names the variable holding the key.
	TokenEnv string `yaml:"token_env,omitempty"`
	// Endpoint is the GraphQL URL.
	Endpoint string `yaml:"endpoint,omitempty"`
}

// Connection is where this resource talks to.
func (s LinearIssuesSource) Connection() LinearConnection {
	return LinearConnection{TokenEnv: s.TokenEnv, Endpoint: s.Endpoint}
}

// FiltersByState reports whether the version carries entered_at: it does when the state is part of the request.
func (s LinearIssuesSource) FiltersByState() bool { return len(s.States) > 0 || len(s.StateTypes) > 0 }

// LinearPostSource is the source: of every put-only type: as whom, and where.
type LinearPostSource struct {
	TokenEnv string `yaml:"token_env,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

// Connection is where this resource talks to.
func (s LinearPostSource) Connection() LinearConnection {
	return LinearConnection(s)
}

// LinearCommentParams is a linear-comment put's params:.
type LinearCommentParams struct {
	// BodyFile is the comment, a path inside the put's inputs.
	BodyFile string `yaml:"body_file"`
	// From names the get input whose version carries the issue, when more than one input does.
	From string `yaml:"from,omitempty"`
	// Issue is an identifier (ENG-123), for a put with no input that names one.
	Issue string `yaml:"issue,omitempty"`
}

// LinearUpdateParams is a linear-update put's params:. One mutation, so a state move and a label swap land whole or not at all.
type LinearUpdateParams struct {
	// State is a state name in the issue's own team.
	State string `yaml:"state,omitempty"`
	// AddLabels are label names to put on the issue. One the team and workspace do not have is refused, not created.
	AddLabels []string `yaml:"add_labels,omitempty"`
	// RemoveLabels are label names to take off it.
	RemoveLabels []string `yaml:"remove_labels,omitempty"`
	// From names the get input whose version carries the issue, when more than one input does.
	From string `yaml:"from,omitempty"`
	// Issue is an identifier (ENG-123), for a put with no input that names one.
	Issue string `yaml:"issue,omitempty"`
}

// ParseLinearIssuesSource decodes a linear-issues source: strictly and checks it.
func ParseLinearIssuesSource(raw map[string]any) (LinearIssuesSource, error) {
	var source LinearIssuesSource

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	if strings.TrimSpace(source.Team) == "" {
		return source, errors.New("source.team is required: state and label names are per team in Linear, so a filter with no team would match every team's")
	}

	err = validateLinearConnection(source.Connection())
	if err != nil {
		return source, err
	}

	err = validateLinearFilters(source)
	if err != nil {
		return source, err
	}

	return source, nil
}

func validateLinearFilters(source LinearIssuesSource) error {
	if len(source.States) > 0 && len(source.StateTypes) > 0 {
		return errors.New("source.states and source.state_types both filter by state; name one")
	}

	for _, field := range []struct {
		name  string
		names []string
	}{{"states", source.States}, {"labels", source.Labels}} {
		if slices.ContainsFunc(field.names, isBlank) {
			return fmt.Errorf("source.%s: a blank name is on no issue, so nothing would match; remove it", field.name)
		}
	}

	err := oneOfEach("source.state_types", "a Linear state type", source.StateTypes, LinearStateTypes...)
	if err != nil {
		return err
	}

	if source.Assignee != "" && source.Assignee != LinearAssigneeMe && !linearIDPattern.MatchString(source.Assignee) {
		return fmt.Errorf("source.assignee: %q is not @me or a user id; an email would put a person identifier in the pipeline", source.Assignee)
	}

	return nil
}

// ParseLinearPostSource decodes a put-only type's source: strictly and checks it.
func ParseLinearPostSource(raw map[string]any) (LinearPostSource, error) {
	var source LinearPostSource

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	return source, validateLinearConnection(source.Connection())
}

func validateLinearConnection(connection LinearConnection) error {
	if connection.TokenEnv != "" && !envVarPattern.MatchString(connection.TokenEnv) {
		return fmt.Errorf("source.token_env must be the NAME of an environment variable holding the key, not %q — a literal would be hashed into state.db in cleartext", connection.TokenEnv)
	}

	return validateEndpoint(connection.Endpoint, DefaultLinearEndpoint)
}

// ParseLinearCommentParams decodes a linear-comment put's params:.
func ParseLinearCommentParams(raw map[string]any) (LinearCommentParams, error) {
	var params LinearCommentParams

	err := decodeParams(raw, &params)
	if err != nil {
		return params, err
	}

	if strings.TrimSpace(params.BodyFile) == "" {
		return params, errors.New("params.body_file is required: the file holding the comment")
	}

	return params, validateLinearTarget(params.From, params.Issue)
}

// ParseLinearUpdateParams decodes a linear-update put's params:.
func ParseLinearUpdateParams(raw map[string]any) (LinearUpdateParams, error) {
	var params LinearUpdateParams

	err := decodeParams(raw, &params)
	if err != nil {
		return params, err
	}

	if params.State == "" && len(params.AddLabels) == 0 && len(params.RemoveLabels) == 0 {
		return params, errors.New("params: set state, add_labels or remove_labels; with none this changes nothing")
	}

	for _, field := range []struct {
		name  string
		names []string
	}{{"add_labels", params.AddLabels}, {"remove_labels", params.RemoveLabels}} {
		if slices.ContainsFunc(field.names, isBlank) {
			return params, fmt.Errorf("params.%s: a blank label name names no label; remove it", field.name)
		}
	}

	for _, name := range params.AddLabels {
		if slices.Contains(params.RemoveLabels, name) {
			return params, fmt.Errorf("params.add_labels and params.remove_labels both name %q", name)
		}
	}

	return params, validateLinearTarget(params.From, params.Issue)
}

// validateLinearTarget checks how a put says which issue. An across: cell or load_var renders issue after load; the put checks what it rendered to.
func validateLinearTarget(from, issue string) error {
	if from != "" && issue != "" {
		return errors.New("params.from and params.issue both say which issue; name one")
	}

	if issue != "" && !strings.Contains(issue, "{{") && !strings.Contains(issue, "((") && !linearIdentifierPattern.MatchString(issue) {
		return fmt.Errorf("params.issue: %q is not an issue identifier such as ENG-123", issue)
	}

	return nil
}

func isBlank(name string) bool { return strings.TrimSpace(name) == "" }

// LinearIdentifierValid reports whether s is an issue identifier, for the put that resolves it at run time.
func LinearIdentifierValid(s string) bool { return linearIdentifierPattern.MatchString(s) }

// validateLinearResources refuses a linear-* resource whose source: is wrong and a put whose params: are.
func (c *Config) validateLinearResources() error {
	for _, resource := range c.Resources {
		err := c.validateLinearResource(resource)
		if err != nil {
			return err
		}
	}

	for i := range c.Jobs {
		gets := c.linearIssueGets(c.Jobs[i])

		err := c.Jobs[i].visitSteps(func(label string, step *Step) error {
			return c.checkLinearStep(label, step, gets)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateLinearResource(resource Resource) error {
	kind := c.linearKind(resource.Type)
	if kind == "" {
		return nil
	}

	if len(resource.Env) > 0 {
		return fmt.Errorf("resource %q: env: has no effect on a %s resource — it reads exactly one variable, the one source.token_env names; drop env: here", resource.Name, kind)
	}

	var err error

	if kind == LinearIssuesType {
		_, err = ParseLinearIssuesSource(resource.Source)
	} else {
		_, err = ParseLinearPostSource(resource.Source)
	}

	if err != nil {
		return fmt.Errorf("resource %q (%s): %w", resource.Name, kind, err)
	}

	return nil
}

// linearIssueGets is the name of every get in job that fetches a linear-issues resource: the inputs whose version can name an issue.
func (c *Config) linearIssueGets(job Job) []string {
	var names []string

	_ = job.visitSteps(func(_ string, step *Step) error {
		if step.Get == "" {
			return nil
		}

		resource, err := c.FindResource(step.GetResourceName())
		if err == nil && c.linearKind(resource.Type) == LinearIssuesType {
			names = append(names, step.Get)
		}

		return nil
	})

	return names
}

func (c *Config) checkLinearStep(label string, step *Step, gets []string) error {
	name, ok := step.resourceName()
	if !ok || step.Put == "" {
		return nil
	}

	resource, err := c.FindResource(name)
	if err != nil {
		return nil //nolint:nilerr // reported by the rule that owns unknown resources
	}

	kind := c.linearKind(resource.Type)

	var from, issue string

	switch kind {
	case LinearCommentType:
		params, err := ParseLinearCommentParams(step.Params)
		if err != nil {
			return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
		}

		from, issue = params.From, params.Issue
	case LinearUpdateType:
		params, err := ParseLinearUpdateParams(step.Params)
		if err != nil {
			return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
		}

		from, issue = params.From, params.Issue
	default:
		return nil
	}

	err = checkLinearTargetInput(step, gets, from, issue)
	if err != nil {
		return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
	}

	return nil
}

// checkLinearTargetInput refuses a put with no linear-issues get among its inputs to name the issue, unless params.issue does, and a from: naming no such get. Which input a build has fetched by then is a run-time fact.
func checkLinearTargetInput(step *Step, gets []string, from, issue string) error {
	visible := gets
	if !step.InputsAll() {
		visible = slices.DeleteFunc(slices.Clone(step.InputNames()), func(name string) bool { return !slices.Contains(gets, name) })
	}

	if issue == "" && len(visible) == 0 {
		return errors.New("no linear-issues get among its inputs names the issue; list that get in the put's inputs, or set params.issue")
	}

	if from != "" && !slices.Contains(visible, from) {
		return fmt.Errorf("params.from: %q is not a linear-issues get among this put's inputs (%s)", from, strings.Join(visible, ", "))
	}

	return nil
}

// linearPutRefusal is validateResourcePut's answer for a linear-* type: a find cannot be put to, because a put's version would land in the history its own get fans out over.
func linearPutRefusal(label, put, kind string) error {
	if !LinearFinds(kind) {
		return nil
	}

	return fmt.Errorf("%s: put %q targets a %s resource, which only finds work; post with a %s or %s resource instead", label, put, kind, LinearCommentType, LinearUpdateType)
}

// linearKind is the linear-* type typeName resolves to, "" for any other.
func (c *Config) linearKind(typeName string) string {
	resourceType, err := c.FindResourceType(typeName)
	if err != nil {
		return ""
	}

	return resourceType.Config.Linear
}

// LinearConnectionOf is where a linear-* resource talks to, read from its source: without the rest of the checks.
func LinearConnectionOf(raw map[string]any) LinearConnection {
	tokenEnv, _ := raw["token_env"].(string)
	endpoint, _ := raw["endpoint"].(string)

	return LinearConnection{TokenEnv: tokenEnv, Endpoint: endpoint}
}
