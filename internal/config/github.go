package config

// The built-in github-* resource types: their names, the shape of each one's
// source: and put params:, and the load-time rules that need no network.
//
// Five types rather than one, split by what they do, the way the Slack
// built-ins are: two that FIND work (a pull request, a comment) and have no
// out, and three that PUBLISH (a comment, a review, a reaction) and have no
// check. A put
// records its version in the resource's own history (recordPutOrder), so a
// type that both watched and posted would mint a comment-shaped version into
// the history it fans out over — and under trigger: true, a post would
// trigger the build that posts again.
//
// All five are Go rather than YAML: steps calls GitHub itself and unpacks the
// tree itself, so nothing needs gh, git or curl on any machine, and a fetched
// tree is an ordinary artifact on this one — placed and containerized steps
// read it the way they read any other.

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// The built-in GitHub type names.
const (
	GitHubPRsType       = "github-prs"
	GitHubCommentsType  = "github-comments"
	GitHubPRCommentType = "github-pr-comment"
	GitHubPRReviewType  = "github-pr-review"
	GitHubReactionType  = "github-reaction"
)

// GitHubTypes is every github-* type, in the order the docs list them.
func GitHubTypes() []string {
	return []string{GitHubPRsType, GitHubCommentsType, GitHubPRCommentType, GitHubPRReviewType, GitHubReactionType}
}

// GitHubFinds reports whether kind is one of the two types a get reads.
func GitHubFinds(kind string) bool {
	return kind == GitHubPRsType || kind == GitHubCommentsType
}

// DefaultGitHubTokenEnv is the variable a github-* resource reads its token
// from when source.token_env does not name another — the one gh itself reads
// first, so `export GH_TOKEN=$(gh auth token)` is the whole setup.
const DefaultGitHubTokenEnv = "GH_TOKEN"

// DefaultGitHubEndpoint is the REST API of github.com. A GitHub Enterprise
// Server's is https://<host>/api/v3.
const DefaultGitHubEndpoint = "https://api.github.com"

// The comment kinds github-comments can watch, as GitHub files them.
const (
	GitHubCommentConversation = "conversation"
	GitHubCommentReview       = "review"
	GitHubCommentIssue        = "issue"
)

// GitHubConnection is where a github-* resource talks to and as whom, common
// to every source: shape.
type GitHubConnection struct {
	Repo     string
	TokenEnv string
	Endpoint string
}

// Token is the variable name the resource reads.
func (c GitHubConnection) Token() string {
	if c.TokenEnv == "" {
		return DefaultGitHubTokenEnv
	}

	return c.TokenEnv
}

// API is the REST base, without a trailing slash.
func (c GitHubConnection) API() string {
	if c.Endpoint == "" {
		return DefaultGitHubEndpoint
	}

	return strings.TrimSuffix(c.Endpoint, "/")
}

// GitHubPRsSource is a github-prs resource's source:. Every filter narrows:
// a pull request is a version only while it is open and matches all of them.
type GitHubPRsSource struct {
	Repo     string `yaml:"repo"`
	TokenEnv string `yaml:"token_env,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
	// Author is who opened it. "@me" is the token's own user.
	Author string `yaml:"author,omitempty"`
	// Assignee is someone it is assigned to. "@me" is the token's own user.
	Assignee string `yaml:"assignee,omitempty"`
	// ReviewRequested is someone whose review it directly requests; GitHub
	// clears the request once they submit one. "@me" is the token's own user.
	ReviewRequested string `yaml:"review_requested,omitempty"`
	// Labels must all be on it.
	Labels []string `yaml:"labels,omitempty"`
	// Base is the branch it would merge into.
	Base string `yaml:"base,omitempty"`
	// Draft, when set, keeps only drafts (true) or only ready ones (false).
	Draft *bool `yaml:"draft,omitempty"`
	// Checkout, true unless set false, lays the tree at the version's commit
	// into the artifact beside the metadata files.
	Checkout *bool `yaml:"checkout,omitempty"`
}

// Connection is where this resource talks to.
func (s GitHubPRsSource) Connection() GitHubConnection {
	return GitHubConnection{Repo: s.Repo, TokenEnv: s.TokenEnv, Endpoint: s.Endpoint}
}

// WantsTree reports whether a get lays down the tree.
func (s GitHubPRsSource) WantsTree() bool { return s.Checkout == nil || *s.Checkout }

// GitHubCommentsSource is a github-comments resource's source:.
type GitHubCommentsSource struct {
	Repo     string `yaml:"repo"`
	TokenEnv string `yaml:"token_env,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
	// Author is who wrote the comment, compared without regard to case, as
	// GitHub compares logins. "@me" is the token's own user.
	Author string `yaml:"author,omitempty"`
	// Body is an RE2 pattern the comment's text must contain a match for;
	// anchor it with ^ to mean "starts with".
	Body string `yaml:"body,omitempty"`
	// Kinds are which comments to watch: conversation (a pull request's
	// timeline), review (inline, on a line of its diff), issue (an issue's
	// timeline). Default conversation and review.
	Kinds []string `yaml:"kinds,omitempty"`
	// Checkout, true unless set false, lays the tree of the pull request a
	// comment sits on into the artifact. An issue has no tree.
	Checkout *bool `yaml:"checkout,omitempty"`
	// SkipReacted is a reaction that, left by the token's own user, marks a
	// comment done: check does not report it. The mark lives on GitHub, so
	// it outlasts a wiped state database, whose first check would otherwise
	// report a page of commands already answered.
	SkipReacted string `yaml:"skip_reacted,omitempty"`
}

// Connection is where this resource talks to.
func (s GitHubCommentsSource) Connection() GitHubConnection {
	return GitHubConnection{Repo: s.Repo, TokenEnv: s.TokenEnv, Endpoint: s.Endpoint}
}

// WantsTree reports whether a get lays down the pull request's tree.
func (s GitHubCommentsSource) WantsTree() bool { return s.Checkout == nil || *s.Checkout }

// WatchedKinds is Kinds with its default filled in.
func (s GitHubCommentsSource) WatchedKinds() []string {
	if len(s.Kinds) == 0 {
		return []string{GitHubCommentConversation, GitHubCommentReview}
	}

	return s.Kinds
}

// GitHubPostSource is the source: of every put-only type: where to post, and
// as whom.
type GitHubPostSource struct {
	Repo     string `yaml:"repo"`
	TokenEnv string `yaml:"token_env,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

// Connection is where this resource talks to.
func (s GitHubPostSource) Connection() GitHubConnection {
	return GitHubConnection(s)
}

// GitHubPostParams is a github-pr-comment put's params:, and the part of a
// github-pr-review put's params: they share.
type GitHubPostParams struct {
	// BodyFile is the text to post, a path inside the put's inputs.
	BodyFile string `yaml:"body_file"`
	// From names the get input whose version carries the pull request's
	// number. Needed only when more than one input carries one.
	From string `yaml:"from,omitempty"`
	// Number is the pull request, for a put with no input that names one.
	Number string `yaml:"number,omitempty"`
	// InThread answers the comment a github-comments get fetched where it was
	// written: an inline review comment in its thread, any other kind in the
	// conversation, which has no threads — so a pipeline watching both kinds
	// sets it once. From then names that get.
	InThread bool `yaml:"in_thread,omitempty"`
}

// GitHubReviewParams is a github-pr-review put's params:.
type GitHubReviewParams struct {
	BodyFile string `yaml:"body_file"`
	From     string `yaml:"from,omitempty"`
	Number   string `yaml:"number,omitempty"`
	// Event is comment (the default), approve, request_changes, or pending:
	// a draft only the token's user can see until they submit it, which
	// replaces that user's own earlier draft.
	Event string `yaml:"event,omitempty"`
	// CommentsFile is a JSON array of comments on lines of the diff, posted
	// as part of the same review: [{path, line, body}], with start_line for
	// a range and side LEFT for a removed line. A path inside the put's
	// inputs, like body_file. An empty array posts the body alone.
	CommentsFile string `yaml:"comments_file,omitempty"`
}

// Post is the part of a review's params: every put shares.
func (p GitHubReviewParams) Post() GitHubPostParams {
	return GitHubPostParams{BodyFile: p.BodyFile, From: p.From, Number: p.Number}
}

// GitHubReactionParams is a github-reaction put's params:. The comment is
// always a github-comments get's: a reaction marks a command, and only that
// type's version names one.
type GitHubReactionParams struct {
	// Add is the reaction to put on the comment.
	Add string `yaml:"add,omitempty"`
	// Remove is a reaction of the token's own user to take off it, after Add
	// is on, so the comment is never without a mark. Anybody else's stays.
	Remove string `yaml:"remove,omitempty"`
	// From names the github-comments get whose comment this marks. Needed
	// only when more than one input carries one.
	From string `yaml:"from,omitempty"`
}

// githubReactions is every reaction GitHub takes on a comment. Its names,
// not Slack's: there is no white_check_mark.
var githubReactions = []string{"+1", "-1", "laugh", "confused", "heart", "hooray", "rocket", "eyes"} //nolint:gochecknoglobals // read-only table

// The review events a put may name, as the params: spell them.
var githubReviewEvents = []string{"comment", "approve", "request_changes", "pending"} //nolint:gochecknoglobals // read-only table

// ParseGitHubPRsSource decodes a github-prs source: strictly and checks it.
func ParseGitHubPRsSource(raw map[string]any) (GitHubPRsSource, error) {
	var source GitHubPRsSource

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	err = validateGitHubConnection(source.Connection())
	if err != nil {
		return source, err
	}

	for field, login := range map[string]string{"author": source.Author, "assignee": source.Assignee, "review_requested": source.ReviewRequested} {
		err = validateGitHubLogin(field, login)
		if err != nil {
			return source, err
		}
	}

	for _, label := range source.Labels {
		if strings.TrimSpace(label) == "" || strings.ContainsAny(label, "\"\n") {
			return source, fmt.Errorf("source.labels: %q is not a label name GitHub search can quote", label)
		}
	}

	if source.Base != "" && strings.ContainsAny(source.Base, " \t\n\"") {
		return source, fmt.Errorf("source.base: %q is not a branch name", source.Base)
	}

	return source, nil
}

// ParseGitHubCommentsSource decodes a github-comments source: strictly and
// checks it, compiling body: so a bad pattern is a load error.
func ParseGitHubCommentsSource(raw map[string]any) (GitHubCommentsSource, *regexp.Regexp, error) {
	var source GitHubCommentsSource

	err := unquotedReaction(raw, "source", "skip_reacted")
	if err != nil {
		return source, nil, err
	}

	err = decodeSource(raw, &source)
	if err != nil {
		return source, nil, err
	}

	err = validateGitHubConnection(source.Connection())
	if err != nil {
		return source, nil, err
	}

	err = validateGitHubLogin("author", source.Author)
	if err != nil {
		return source, nil, err
	}

	known := []string{GitHubCommentConversation, GitHubCommentReview, GitHubCommentIssue}

	for _, kind := range source.Kinds {
		if !slices.Contains(known, kind) {
			return source, nil, fmt.Errorf("source.kinds: %q is not a comment kind (%s)", kind, strings.Join(known, ", "))
		}
	}

	pattern, err := regexp.Compile(source.Body)
	if err != nil {
		return source, nil, fmt.Errorf("source.body: %w", err)
	}

	return source, pattern, validateGitHubReaction("source.skip_reacted", source.SkipReacted)
}

// unquotedReaction refuses a reaction YAML has already read as a number:
// +1 and -1 unquoted are integers, and +1 comes back as "1", which is no
// reaction at all and would be reported as one with no hint why.
func unquotedReaction(raw map[string]any, section string, fields ...string) error {
	for _, field := range fields {
		if value, ok := raw[field]; ok && value != nil {
			if _, isString := value.(string); !isString {
				return fmt.Errorf(`%s.%s: %v is a number to YAML, not a reaction; quote it: "+1"`, section, field, value)
			}
		}
	}

	return nil
}

func validateGitHubReaction(field, reaction string) error {
	if reaction == "" || slices.Contains(githubReactions, reaction) {
		return nil
	}

	return fmt.Errorf("%s: %q is not a GitHub reaction (%s)", field, reaction, strings.Join(githubReactions, ", "))
}

// ParseGitHubPostSource decodes a put-only type's source: strictly and
// checks it.
func ParseGitHubPostSource(raw map[string]any) (GitHubPostSource, error) {
	var source GitHubPostSource

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	return source, validateGitHubConnection(source.Connection())
}

// ParseGitHubPostParams decodes a github-pr-comment put's params:.
func ParseGitHubPostParams(raw map[string]any) (GitHubPostParams, error) {
	var params GitHubPostParams

	err := decodeParams(raw, &params)
	if err != nil {
		return params, err
	}

	return params, validateGitHubPost(params)
}

// ParseGitHubReviewParams decodes a github-pr-review put's params:.
func ParseGitHubReviewParams(raw map[string]any) (GitHubReviewParams, error) {
	var params GitHubReviewParams

	err := decodeParams(raw, &params)
	if err != nil {
		return params, err
	}

	if params.Event != "" && !slices.Contains(githubReviewEvents, params.Event) {
		return params, fmt.Errorf("params.event: %q is not a review event (%s)", params.Event, strings.Join(githubReviewEvents, ", "))
	}

	return params, validateGitHubPost(params.Post())
}

// ParseGitHubReactionParams decodes a github-reaction put's params:.
func ParseGitHubReactionParams(raw map[string]any) (GitHubReactionParams, error) {
	var params GitHubReactionParams

	err := unquotedReaction(raw, "params", "add", "remove")
	if err != nil {
		return params, err
	}

	err = decodeParams(raw, &params)
	if err != nil {
		return params, err
	}

	switch {
	case params.Add == "" && params.Remove == "":
		return params, errors.New("params.add or params.remove is required: the reaction to put on the comment, or the token user's own one to take off")
	case params.Add == params.Remove:
		return params, fmt.Errorf("params.add and params.remove name the same reaction, %q; the remove would undo the add", params.Add)
	}

	err = validateGitHubReaction("params.add", params.Add)
	if err != nil {
		return params, err
	}

	return params, validateGitHubReaction("params.remove", params.Remove)
}

func decodeParams(raw map[string]any, out any) error {
	if raw == nil {
		raw = map[string]any{}
	}

	err := decodeSource(raw, out)
	if err != nil {
		return errors.New(strings.Replace(err.Error(), "source:", "params:", 1))
	}

	return nil
}

func validateGitHubPost(params GitHubPostParams) error {
	if params.BodyFile == "" {
		return errors.New("params.body_file is required: the file, inside the put's inputs, whose text is posted")
	}

	if params.From != "" && params.Number != "" {
		return errors.New("params.from and params.number both say which pull request; name one")
	}

	if params.InThread && params.Number != "" {
		return errors.New("params.in_thread answers a comment a github-comments get fetched, and params.number names a pull request with no comment to answer; drop one")
	}

	return nil
}

// githubRepoPattern is owner/name as GitHub allows them.
var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`) //nolint:gochecknoglobals // compiled once, read-only

// githubLoginPattern is a GitHub login, a bot's included, or @me.
var githubLoginPattern = regexp.MustCompile(`^(@me|[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(\[bot\])?)$`) //nolint:gochecknoglobals // compiled once, read-only

func validateGitHubConnection(connection GitHubConnection) error {
	if !githubRepoPattern.MatchString(connection.Repo) {
		return fmt.Errorf("source.repo must be owner/name, not %q", connection.Repo)
	}

	if connection.TokenEnv != "" && !envVarPattern.MatchString(connection.TokenEnv) {
		return fmt.Errorf("source.token_env must be the NAME of an environment variable holding the token, not %q — a literal would be hashed into state.db in cleartext", connection.TokenEnv)
	}

	if connection.Endpoint != "" {
		parsed, err := url.Parse(connection.Endpoint)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return fmt.Errorf("source.endpoint must be an http(s) URL such as %s, not %q", DefaultGitHubEndpoint, connection.Endpoint)
		}
	}

	return nil
}

// validateGitHubLogin is what keeps a filter one search qualifier: a value
// with a space in it would be read by GitHub as a second one.
func validateGitHubLogin(field, login string) error {
	if login == "" || githubLoginPattern.MatchString(login) {
		return nil
	}

	return fmt.Errorf("source.%s: %q is not a GitHub login (or @me)", field, login)
}

// validateGitHubResources refuses a resource_types: entry that takes a
// built-in's name, a github-* resource whose source: is wrong, and a put
// whose params: are.
func (c *Config) validateGitHubResources() error {
	for _, rt := range c.ResourceTypes {
		if slices.Contains(GitHubTypes(), rt.Name) && rt.Config.GitHub == "" {
			return fmt.Errorf("resource_type %q: the name is built in — a GitHub resource needs no resource_types: entry, so name this type something else", rt.Name)
		}
	}

	for _, resource := range c.Resources {
		err := c.validateGitHubResource(resource)
		if err != nil {
			return err
		}
	}

	return c.validateGitHubSteps()
}

func (c *Config) validateGitHubResource(resource Resource) error {
	kind := c.githubKind(resource.Type)
	if kind == "" {
		return nil
	}

	if len(resource.Env) > 0 {
		return fmt.Errorf("resource %q: env: has no effect on a %s resource — it reads exactly one variable, the one source.token_env names; drop env: here", resource.Name, kind)
	}

	var err error

	switch kind {
	case GitHubPRsType:
		_, err = ParseGitHubPRsSource(resource.Source)
	case GitHubCommentsType:
		_, _, err = ParseGitHubCommentsSource(resource.Source)
	default:
		_, err = ParseGitHubPostSource(resource.Source)
	}

	if err != nil {
		return fmt.Errorf("resource %q (%s): %w", resource.Name, kind, err)
	}

	return nil
}

// validateGitHubSteps checks each get's and put's params: against the type
// it reaches: a find takes none, a post takes its own.
func (c *Config) validateGitHubSteps() error {
	for i := range c.Jobs {
		comments := c.githubCommentGets(c.Jobs[i])

		err := c.Jobs[i].visitSteps(func(label string, step *Step) error {
			return c.checkGitHubStep(label, step, comments)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// githubCommentGets is the name of every get in job that fetches a
// github-comments resource, aliased or not: the names a reaction's or a
// threaded reply's inputs can carry a comment under.
func (c *Config) githubCommentGets(job Job) []string {
	var names []string

	_ = job.visitSteps(func(_ string, step *Step) error {
		if step.Get == "" {
			return nil
		}

		resource, err := c.FindResource(step.GetResourceName())
		if err == nil && c.githubKind(resource.Type) == GitHubCommentsType {
			names = append(names, step.Get)
		}

		return nil
	})

	return names
}

func checkReactionStep(step *Step, comments []string) error {
	params, err := ParseGitHubReactionParams(step.Params)
	if err != nil {
		return err
	}

	return checkCommentInput(step, comments, params.From, "react to")
}

func checkPRCommentStep(step *Step, comments []string) error {
	params, err := ParseGitHubPostParams(step.Params)
	if err != nil || !params.InThread {
		return err
	}

	return checkCommentInput(step, comments, params.From, "answer")
}

// checkCommentInput refuses a put that needs a comment when no input of it
// can name one, and a from: naming one that cannot. Several candidates are
// left to the put: which of them a build has fetched by then is a run-time
// fact.
func checkCommentInput(step *Step, comments []string, from, verb string) error {
	visible := comments
	if !step.InputsAll() {
		visible = slices.DeleteFunc(slices.Clone(step.InputNames()), func(name string) bool { return !slices.Contains(comments, name) })
	}

	if len(visible) == 0 {
		return fmt.Errorf("no github-comments get among its inputs names the comment to %s; list that get in the put's inputs", verb)
	}

	if from != "" && !slices.Contains(visible, from) {
		return fmt.Errorf("params.from: %q is not a github-comments get among this put's inputs (%s)", from, strings.Join(visible, ", "))
	}

	return nil
}

func (c *Config) checkGitHubStep(label string, step *Step, comments []string) error {
	name, ok := step.resourceName()
	if !ok {
		return nil
	}

	resource, err := c.FindResource(name)
	if err != nil {
		return nil //nolint:nilerr // reported by the rule that owns unknown resources
	}

	kind := c.githubKind(resource.Type)

	switch {
	case kind == "":
		return nil
	case step.Get != "" && len(step.Params) > 0:
		return fmt.Errorf("%s: get %q: a %s get takes no params; what it fetches is set on the resource's source instead", label, step.Get, kind)
	case kind == GitHubPRCommentType:
		err = checkPRCommentStep(step, comments)
	case kind == GitHubPRReviewType:
		_, err = ParseGitHubReviewParams(step.Params)
	case kind == GitHubReactionType:
		err = checkReactionStep(step, comments)
	}

	if err != nil {
		return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
	}

	return nil
}

// githubPutRefusal is validateResourcePut's answer for a github-* type: a
// find cannot be put to, because a put's version would land in the history
// its own get fans out over.
func githubPutRefusal(label, put, kind string) error {
	if !GitHubFinds(kind) {
		return nil
	}

	return fmt.Errorf("%s: put %q targets a %s resource, which only finds work; post with a %s, %s or %s resource instead",
		label, put, kind, GitHubPRCommentType, GitHubPRReviewType, GitHubReactionType)
}

// githubKind is the github-* type typeName resolves to, "" for any other.
func (c *Config) githubKind(typeName string) string {
	resourceType, err := c.FindResourceType(typeName)
	if err != nil {
		return ""
	}

	return resourceType.Config.GitHub
}

// GitHubConnectionOf is where a github-* resource talks to, read from its
// source: without the rest of the checks — for a caller that has already
// loaded the pipeline and wants only the token's name.
func GitHubConnectionOf(raw map[string]any) GitHubConnection {
	repo, _ := raw["repo"].(string)
	tokenEnv, _ := raw["token_env"].(string)
	endpoint, _ := raw["endpoint"].(string)

	return GitHubConnection{Repo: repo, TokenEnv: tokenEnv, Endpoint: endpoint}
}
