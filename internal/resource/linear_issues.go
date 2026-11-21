package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
)

// linearPageSize is a page of issues or labels; linearIssueLimit is what one check will read before it refuses, since a silently shortened list is a version that never builds.
const (
	linearPageSize   = 100
	linearIssueLimit = 1000
	// linearHistoryDepth is how many history entries each issue's check reads to find when it last entered a filter state: the newest ones, since Linear's schema documents history(first:, orderBy: createdAt) as newest first and history(last:) as the oldest.
	linearHistoryDepth = 20
)

// The files a linear-issues get writes beside the ones github-issues shares (issue.json, issue.title, issue.url, issue.comments.json, issue.md).
const (
	linearIDFile         = "issue.id"
	linearIdentifierFile = "issue.identifier"
	linearBranchFile     = "issue.branch"
)

const linearTeamQuery = `query($team: String!) {
  teams(filter: {or: [{key: {eq: $team}}, {name: {eq: $team}}]}, first: 2) {
    nodes { id key name states(first: 250) { nodes { id name type } } }
  }
}`

// linearLabelsQuery is the labels a team's issues can carry: the team's own and the workspace's.
const linearLabelsQuery = `query($team: ID!, $first: Int!, $after: String) {
  issueLabels(first: $first, after: $after, filter: {or: [{team: {id: {eq: $team}}}, {team: {null: true}}]}) {
    pageInfo { hasNextPage endCursor }
    nodes { id name }
  }
}`

// linearProjectsQuery is the projects of a team a source's project names, by id or by name without regard to case, as states and labels match; first: 2 is enough to tell one from several.
const linearProjectsQuery = `query($team: ID!, $id: ID!, $name: String!) {
  projects(filter: {accessibleTeams: {some: {id: {eq: $team}}}, or: [{id: {eq: $id}}, {name: {eqIgnoreCase: $name}}]}, first: 2) {
    nodes { id name }
  }
}`

const linearIssuesQuery = `query($filter: IssueFilter, $first: Int!, $after: String, $history: Int!, $withHistory: Boolean!) {
  issues(filter: $filter, first: $first, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { identifier createdAt history(first: $history, orderBy: createdAt) @include(if: $withHistory) { nodes { createdAt fromState { id type } toState { id type } } } }
  }
}`

const linearIssueQuery = `query($id: String!) {
  issue(id: $id) {
    id identifier title description url branchName createdAt updatedAt priority
    state { id name type }
    team { id key name }
    project { id name }
    assignee { id }
    labels(first: 100) { nodes { id name } }
  }
}`

const linearCommentsQuery = `query($id: String!, $after: String) {
  issue(id: $id) {
    comments(first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { id body createdAt parent { id } user { name } }
    }
  }
}`

type linearTeam struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	States struct {
		Nodes []linearState `json:"nodes"`
	} `json:"states"`
}

type linearState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type linearLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// resolveTeam is the one team a key or name names, with its states.
func (c *linearClient) resolveTeam(ctx context.Context, team string) (linearTeam, error) {
	var answer struct {
		Teams struct {
			Nodes []linearTeam `json:"nodes"`
		} `json:"teams"`
	}

	err := c.query(ctx, linearTeamQuery, map[string]any{"team": team}, &answer)
	if err != nil {
		return linearTeam{}, err
	}

	switch len(answer.Teams.Nodes) {
	case 1:
		return answer.Teams.Nodes[0], nil
	case 0:
		return linearTeam{}, fmt.Errorf("source.team: Linear has no team with the key or name %q", team)
	default:
		return linearTeam{}, fmt.Errorf("source.team: %q names more than one team; use its key", team)
	}
}

// labelsOf is every label an issue of the team can carry, paged to the end: a label past a page would be refused as missing.
func (c *linearClient) labelsOf(ctx context.Context, teamID string) ([]linearLabel, error) {
	var all []linearLabel

	var after any

	for {
		var answer struct {
			IssueLabels struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []linearLabel `json:"nodes"`
			} `json:"issueLabels"`
		}

		err := c.query(ctx, linearLabelsQuery, map[string]any{"team": teamID, "first": linearPageSize, "after": after}, &answer)
		if err != nil {
			return nil, err
		}

		all = append(all, answer.IssueLabels.Nodes...)

		if !answer.IssueLabels.PageInfo.HasNextPage || answer.IssueLabels.PageInfo.EndCursor == "" {
			return all, nil
		}

		after = answer.IssueLabels.PageInfo.EndCursor
	}
}

// projectID resolves a project name or id to the one project of team it names, refusing one the team does not have by name rather than matching nothing; field is where the pipeline wrote it.
func (c *linearClient) projectID(ctx context.Context, field string, team linearTeam, project string) (string, error) {
	var answer struct {
		Projects struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"projects"`
	}

	err := c.query(ctx, linearProjectsQuery, map[string]any{"team": team.ID, "id": project, "name": project}, &answer)
	if err != nil {
		return "", err
	}

	switch len(answer.Projects.Nodes) {
	case 1:
		return answer.Projects.Nodes[0].ID, nil
	case 0:
		return "", fmt.Errorf("%s: team %s has no project named %q", field, team.Key, project)
	default:
		return "", fmt.Errorf("%s: %q names more than one of team %s's projects; use its id", field, project, team.Key)
	}
}

// stateIDs resolves state names in team to ids, refusing a name the team does not have by name rather than matching nothing.
func stateIDs(team linearTeam, names []string) ([]string, error) {
	ids := make([]string, 0, len(names))

	for _, name := range names {
		i := slices.IndexFunc(team.States.Nodes, func(state linearState) bool { return strings.EqualFold(state.Name, name) })
		if i < 0 {
			return nil, fmt.Errorf("source.states: team %s has no state named %q (it has: %s)", team.Key, name, stateNames(team))
		}

		ids = append(ids, team.States.Nodes[i].ID)
	}

	return ids, nil
}

func stateNames(team linearTeam) string {
	names := make([]string, len(team.States.Nodes))
	for i, state := range team.States.Nodes {
		names[i] = state.Name
	}

	return strings.Join(names, ", ")
}

// labelIDs resolves label names to ids, matched without regard to case as GitHub matches label names, refusing one that does not exist: Linear does not create a label on use, and a typo should be a red step.
func labelIDs(field string, labels []linearLabel, names []string) ([]string, error) {
	ids := make([]string, 0, len(names))

	for _, name := range names {
		i := slices.IndexFunc(labels, func(label linearLabel) bool { return strings.EqualFold(label.Name, name) })
		if i < 0 {
			return nil, fmt.Errorf("%s: Linear has no label named %q on this team or in the workspace", field, name)
		}

		ids = append(ids, labels[i].ID)
	}

	return ids, nil
}

// linearFilter is the IssueFilter for a source, its names already resolved.
func linearFilter(source config.LinearIssuesSource, team linearTeam, stateIDs, labelIDs []string, projectID string) map[string]any {
	filter := map[string]any{"team": map[string]any{"id": map[string]any{"eq": team.ID}}}

	switch {
	case len(stateIDs) > 0:
		filter["state"] = map[string]any{"id": map[string]any{"in": stateIDs}}
	case len(source.StateTypes) > 0:
		filter["state"] = map[string]any{"type": map[string]any{"in": source.StateTypes}}
	}

	if len(labelIDs) > 0 {
		all := make([]map[string]any, len(labelIDs))
		for i, id := range labelIDs {
			all[i] = map[string]any{"labels": map[string]any{"some": map[string]any{"id": map[string]any{"eq": id}}}}
		}

		filter["and"] = all
	}

	if projectID != "" {
		filter["project"] = map[string]any{"id": map[string]any{"eq": projectID}}
	}

	switch source.Assignee {
	case "":
	case config.LinearAssigneeMe:
		filter["assignee"] = map[string]any{"isMe": map[string]any{"eq": true}}
	default:
		filter["assignee"] = map[string]any{"id": map[string]any{"eq": source.Assignee}}
	}

	return filter
}

// sourceFilter resolves the names a source gives into the IssueFilter that asks for them, and returns the state ids it resolved, which the version's entered_at is read against.
func (c *linearClient) sourceFilter(ctx context.Context, source config.LinearIssuesSource) (map[string]any, []string, error) {
	team, err := c.resolveTeam(ctx, source.Team)
	if err != nil {
		return nil, nil, err
	}

	states, err := stateIDs(team, source.States)
	if err != nil {
		return nil, nil, err
	}

	var labels []string

	if len(source.Labels) > 0 {
		known, err := c.labelsOf(ctx, team.ID)
		if err != nil {
			return nil, nil, err
		}

		labels, err = labelIDs("source.labels", known, source.Labels)
		if err != nil {
			return nil, nil, err
		}
	}

	var project string

	if source.Project != "" {
		project, err = c.projectID(ctx, "source.project", team, source.Project)
		if err != nil {
			return nil, nil, err
		}
	}

	return linearFilter(source, team, states, labels, project), states, nil
}

type linearIssueNode struct {
	Identifier string `json:"identifier"`
	CreatedAt  string `json:"createdAt"`
	History    struct {
		Nodes []linearHistoryEntry `json:"nodes"`
	} `json:"history"`
}

type linearHistoryEntry struct {
	CreatedAt string       `json:"createdAt"`
	FromState *linearState `json:"fromState"`
	ToState   *linearState `json:"toState"`
}

func linearIssuesCheck(ctx context.Context, raw map[string]any) ([]map[string]any, error) {
	source, err := config.ParseLinearIssuesSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	client, err := newLinearClient(ctx, source.Connection())
	if err != nil {
		return nil, err
	}

	filter, states, err := client.sourceFilter(ctx, source)
	if err != nil {
		return nil, err
	}

	nodes, err := client.issues(ctx, filter, source.FiltersByState())
	if err != nil {
		return nil, err
	}

	// Oldest first, as github-prs reports: the identifier breaks a tie, so the order is the same on every poll.
	slices.SortFunc(nodes, func(a, b linearIssueNode) int {
		return strings.Compare(a.CreatedAt+" "+a.Identifier, b.CreatedAt+" "+b.Identifier)
	})

	versions := make([]map[string]any, 0, len(nodes))

	for _, node := range nodes {
		version := map[string]any{"identifier": node.Identifier}
		if source.FiltersByState() {
			version["entered_at"] = enteredAt(node, states, source.StateTypes)
		}

		versions = append(versions, version)
	}

	return versions, nil
}

// issues pages the matching issues to the end, and fails past linearIssueLimit rather than truncating.
func (c *linearClient) issues(ctx context.Context, filter map[string]any, withHistory bool) ([]linearIssueNode, error) {
	var all []linearIssueNode

	var after any

	for {
		var answer struct {
			Issues struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []linearIssueNode `json:"nodes"`
			} `json:"issues"`
		}

		err := c.query(ctx, linearIssuesQuery, map[string]any{"filter": filter, "first": linearPageSize, "after": after, "history": linearHistoryDepth, "withHistory": withHistory}, &answer)
		if err != nil {
			return nil, err
		}

		all = append(all, answer.Issues.Nodes...)

		if len(all) > linearIssueLimit {
			return nil, fmt.Errorf("more than %d issues match this source; narrow it with states, labels or a project, since a list cut short is a version that never builds", linearIssueLimit)
		}

		if !answer.Issues.PageInfo.HasNextPage || answer.Issues.PageInfo.EndCursor == "" {
			return all, nil
		}

		after = answer.Issues.PageInfo.EndCursor
	}
}

// enteredAt is when the issue last moved into the filter set from outside it: the latest history entry whose destination is a filter state and whose origin is not, so Todo to Ready under states: [Todo, Ready] is no new version. A null origin (the entry that created the issue, or a state no longer readable) counts as outside. An issue with no such entry in those read (created straight into the set, or moved there more than linearHistoryDepth changes ago) falls back to when it was created, which is stable, so it comes back as a new version at most once.
func enteredAt(node linearIssueNode, stateIDs, stateTypes []string) string {
	watched := func(state *linearState) bool {
		return state != nil && (slices.Contains(stateIDs, state.ID) || slices.Contains(stateTypes, state.Type))
	}

	latest := ""

	for _, entry := range node.History.Nodes {
		// Linear writes every createdAt as UTC in one format, so the strings sort as the moments do.
		if watched(entry.ToState) && !watched(entry.FromState) && entry.CreatedAt > latest {
			latest = entry.CreatedAt
		}
	}

	if latest == "" {
		return node.CreatedAt
	}

	return latest
}

func linearIssuesIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, err := config.ParseLinearIssuesSource(raw)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return fetchLinearIssue(ctx, source.Connection(), version, destDir)
}

// fetchLinearIssue writes the files every get of an issue writes, whichever type's version names it, so a linear-comment or linear-update finds the issue from either input the same way.
func fetchLinearIssue(ctx context.Context, connection config.LinearConnection, version map[string]any, destDir string) error {
	identifier := versionField(version, "identifier")
	if !config.LinearIdentifierValid(identifier) {
		return fmt.Errorf("version %v names no issue identifier", version)
	}

	client, err := newLinearClient(ctx, connection)
	if err != nil {
		return err
	}

	// The issue as it is now, whatever the version: an issue has no commit to pin it to, and a comment written since is part of the work.
	var answer struct {
		Issue json.RawMessage `json:"issue"`
	}

	err = client.query(ctx, linearIssueQuery, map[string]any{"id": identifier}, &answer)
	if err != nil {
		return err
	}

	var issue struct {
		ID          string `json:"id"`
		Identifier  string `json:"identifier"`
		Title       string `json:"title"`
		Description string `json:"description"`
		URL         string `json:"url"`
		BranchName  string `json:"branchName"`
	}

	if len(answer.Issue) == 0 || string(answer.Issue) == "null" {
		return fmt.Errorf("no Linear issue %s", identifier)
	}

	err = json.Unmarshal(answer.Issue, &issue)
	if err != nil {
		return fmt.Errorf("linear: issue %s: %w", identifier, err)
	}

	comments, commentsJSON, err := client.comments(ctx, identifier)
	if err != nil {
		return err
	}

	reportFacts(ctx, []events.Fact{{Name: "url", Value: issue.URL}, {Name: "title", Value: issue.Title}})

	return writeMetadata(destDir, map[string][]byte{
		issueJSONFile:        answer.Issue,
		linearIDFile:         []byte(issue.ID),
		linearIdentifierFile: []byte(issue.Identifier),
		issueTitleFile:       []byte(issue.Title),
		issueURLFile:         []byte(issue.URL),
		linearBranchFile:     []byte(issue.BranchName),
		issueCommentsFile:    commentsJSON,
		issueMarkdownFile:    []byte(linearIssueMarkdown(issue.Identifier, issue.Title, issue.URL, issue.Description, comments)),
	})
}

type linearComment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

// comments is every comment on an issue, oldest first, decoded and as Linear sent them: threads are flattened, with parent kept on each.
func (c *linearClient) comments(ctx context.Context, identifier string) ([]linearComment, []byte, error) {
	type entry struct {
		comment linearComment
		raw     json.RawMessage
	}

	var entries []entry

	var after any

	for {
		var answer struct {
			Issue *struct {
				Comments struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []json.RawMessage `json:"nodes"`
				} `json:"comments"`
			} `json:"issue"`
		}

		err := c.query(ctx, linearCommentsQuery, map[string]any{"id": identifier, "after": after}, &answer)
		if err != nil {
			return nil, nil, err
		}

		if answer.Issue == nil {
			return nil, nil, errors.New("no Linear issue " + identifier)
		}

		for _, raw := range answer.Issue.Comments.Nodes {
			var comment linearComment

			err := json.Unmarshal(raw, &comment)
			if err != nil {
				return nil, nil, fmt.Errorf("linear: comments on %s: %w", identifier, err)
			}

			entries = append(entries, entry{comment: comment, raw: raw})
		}

		if !answer.Issue.Comments.PageInfo.HasNextPage || answer.Issue.Comments.PageInfo.EndCursor == "" {
			break
		}

		after = answer.Issue.Comments.PageInfo.EndCursor
	}

	slices.SortStableFunc(entries, func(a, b entry) int { return strings.Compare(a.comment.CreatedAt, b.comment.CreatedAt) })

	comments := make([]linearComment, len(entries))
	raws := make([]json.RawMessage, len(entries))

	for i, e := range entries {
		comments[i], raws[i] = e.comment, e.raw
	}

	encoded, err := json.Marshal(raws)
	if err != nil {
		return nil, nil, fmt.Errorf("linear: comments: %w", err)
	}

	return comments, encoded, nil
}

// linearIssueMarkdown is the issue as a planner reads it: identifier and title, page, description, then the discussion, because a decision recorded in a comment is as binding as the description. Authors stay out, so a model reading it is not handed people's names it does not need; they remain in issue.comments.json.
func linearIssueMarkdown(identifier, title, pageURL, description string, comments []linearComment) string {
	if description == "" {
		description = "(no description)"
	}

	var out strings.Builder

	fmt.Fprintf(&out, "# %s: %s\n\n%s\n\n%s\n", identifier, title, pageURL, description)

	for _, comment := range comments {
		fmt.Fprintf(&out, "\n## Comment, %s\n\n%s\n", comment.CreatedAt, comment.Body)
	}

	return out.String()
}
