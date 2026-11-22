package resource

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
)

const linearIssueCreateMutation = `mutation($input: IssueCreateInput!) {
  issueCreate(input: $input) { success issue { id identifier title url } }
}`

const linearViewerQuery = `query { viewer { id } }`

const linearUserQuery = `query($id: ID!) {
  users(filter: {id: {eq: $id}}, first: 1) { nodes { id } }
}`

// linearTitledQuery reads the issues an IssueFilter matches with what reuse: compares; first: is a bound, since a team with that many open issues of one title has a problem reuse cannot fix.
const linearTitledQuery = `query($filter: IssueFilter, $first: Int!) {
  issues(filter: $filter, first: $first) { nodes { identifier title url createdAt } }
}`

// linearClosedStateTypes are the categories reuse: title skips: an issue someone finished or threw away is history, not the work this put is for.
var linearClosedStateTypes = []string{"completed", "canceled"} //nolint:gochecknoglobals // read-only table

type linearCreated struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	CreatedAt  string `json:"createdAt"`
}

// linearIssueOut opens one issue, or with reuse: title returns the open one that already has the title. Every name is resolved before the search and before the create, so a typo fails the put the same way whether or not an issue would have been reused.
func linearIssueOut(ctx context.Context, raw, rawParams map[string]any, _ PutInputs, srcDir string) (map[string]any, error) {
	source, err := config.ParseLinearIssueSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseLinearIssueParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	title, body, err := readIssueFiles(srcDir, params)
	if err != nil {
		return nil, err
	}

	client, err := newLinearClient(ctx, source.Connection())
	if err != nil {
		return nil, err
	}

	team, err := client.resolveTeam(ctx, source.Team)
	if err != nil {
		return nil, err
	}

	input, err := client.issueCreateInput(ctx, team, params)
	if err != nil {
		return nil, err
	}

	if params.Reuse == config.LinearReuseTitle {
		version, found, err := client.reuseTitled(ctx, team, title)
		if err != nil || found {
			return version, err
		}
	}

	input["title"] = title
	input["description"] = body

	return client.createIssue(ctx, team, input)
}

// readIssueFiles reads the title and description before any request, so a bad file costs none.
func readIssueFiles(srcDir string, params config.LinearIssueParams) (string, string, error) {
	title, err := readIssueTitle(srcDir, params.TitleFile)
	if err != nil {
		return "", "", err
	}

	body, err := readPostBody(srcDir, params.BodyFile)

	return title, body, err
}

// createIssue sends the one request that opens the issue.
func (c *linearClient) createIssue(ctx context.Context, team linearTeam, input map[string]any) (map[string]any, error) {
	var answer struct {
		IssueCreate struct {
			Success bool          `json:"success"`
			Issue   linearCreated `json:"issue"`
		} `json:"issueCreate"`
	}

	err := c.mutate(ctx, linearIssueCreateMutation, map[string]any{"input": input}, &answer)
	if err != nil {
		return nil, err
	}

	if !answer.IssueCreate.Success || !config.LinearIdentifierValid(answer.IssueCreate.Issue.Identifier) {
		return nil, fmt.Errorf("linear: issueCreate in team %s did not succeed", team.Key)
	}

	events.Note(ctx, events.NoteInfo, fmt.Sprintf("created %s in team %s", answer.IssueCreate.Issue.Identifier, team.Key))

	return reportIssue(ctx, answer.IssueCreate.Issue, false), nil
}

// reportIssue reports the facts and returns the version, the same for an issue opened and one reused, so nothing after the put can tell them apart but the reused fact.
func reportIssue(ctx context.Context, issue linearCreated, reused bool) map[string]any {
	facts := []events.Fact{{Name: "url", Value: issue.URL}, {Name: "title", Value: issue.Title}, {Name: "identifier", Value: issue.Identifier}}
	if reused {
		facts = append(facts, events.Fact{Name: "reused", Value: "yes"})
	}

	reportFacts(ctx, facts)

	return map[string]any{"identifier": issue.Identifier}
}

// issueCreateInput resolves every name the put gave into the ids issueCreate takes, leaving out what it did not ask for so Linear's own defaults apply.
func (c *linearClient) issueCreateInput(ctx context.Context, team linearTeam, params config.LinearIssueParams) (map[string]any, error) {
	input := map[string]any{"teamId": team.ID}

	err := c.resolveTeamNames(ctx, team, params, input)
	if err != nil {
		return nil, err
	}

	if params.Estimate != nil {
		input["estimate"] = *params.Estimate
	}

	if params.Assignee != "" {
		input["assigneeId"], err = c.userID(ctx, params.Assignee)
		if err != nil {
			return nil, err
		}
	}

	if params.Parent != "" {
		parent, err := c.issueRef(ctx, params.Parent)
		if err != nil {
			return nil, fmt.Errorf("params.parent: %w", err)
		}

		input["parentId"] = parent.ID
	}

	return input, nil
}

// resolveTeamNames sets the ids of the names that belong to the team: its state, its labels, its project.
func (c *linearClient) resolveTeamNames(ctx context.Context, team linearTeam, params config.LinearIssueParams, input map[string]any) error {
	if params.State != "" {
		ids, err := stateIDs(team, []string{params.State})
		if err != nil {
			return fmt.Errorf("params.state: team %s has no state named %q (it has: %s)", team.Key, params.State, stateNames(team))
		}

		input["stateId"] = ids[0]
	}

	if len(params.Labels) > 0 {
		known, err := c.labelsOf(ctx, team.ID)
		if err != nil {
			return err
		}

		input["labelIds"], err = labelIDs("params.labels", known, params.Labels)
		if err != nil {
			return err
		}
	}

	if params.Project != "" {
		id, err := c.projectID(ctx, "params.project", team, params.Project)
		if err != nil {
			return err
		}

		input["projectId"] = id
	}

	return nil
}

// userID is the key's own user for @me, and otherwise the id given once Linear has confirmed someone has it.
func (c *linearClient) userID(ctx context.Context, assignee string) (string, error) {
	if assignee == config.LinearAssigneeMe {
		var answer struct {
			Viewer struct {
				ID string `json:"id"`
			} `json:"viewer"`
		}

		err := c.query(ctx, linearViewerQuery, map[string]any{}, &answer)
		if err != nil {
			return "", err
		}

		if answer.Viewer.ID == "" {
			return "", errors.New("params.assignee: Linear named no user for this key")
		}

		return answer.Viewer.ID, nil
	}

	var answer struct {
		Users struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"users"`
	}

	err := c.query(ctx, linearUserQuery, map[string]any{"id": assignee}, &answer)
	if err != nil {
		return "", err
	}

	if len(answer.Users.Nodes) == 0 {
		return "", fmt.Errorf("params.assignee: Linear has no user %q", assignee)
	}

	return answer.Users.Nodes[0].ID, nil
}

// reuseTitled answers the put with the open issue that has the title, when there is one.
func (c *linearClient) reuseTitled(ctx context.Context, team linearTeam, title string) (map[string]any, bool, error) {
	existing, found, err := c.openIssueTitled(ctx, team, title)
	if err != nil || !found {
		return nil, false, err
	}

	events.Note(ctx, events.NoteInfo, fmt.Sprintf("reused %s: it is open in team %s with this exact title (params.reuse: title), so no issue was created", existing.Identifier, team.Key))

	return reportIssue(ctx, existing, true), true, nil
}

// openIssueTitled is the oldest issue in team whose title is exactly title (Linear's eq is case-sensitive; eqIgnoreCase is the other) and whose state is neither completed nor canceled.
func (c *linearClient) openIssueTitled(ctx context.Context, team linearTeam, title string) (linearCreated, bool, error) {
	filter := map[string]any{
		"team":  map[string]any{"id": map[string]any{"eq": team.ID}},
		"title": map[string]any{"eq": title},
		"state": map[string]any{"type": map[string]any{"nin": linearClosedStateTypes}},
	}

	var answer struct {
		Issues struct {
			Nodes []linearCreated `json:"nodes"`
		} `json:"issues"`
	}

	err := c.query(ctx, linearTitledQuery, map[string]any{"filter": filter, "first": linearPageSize}, &answer)
	if err != nil {
		return linearCreated{}, false, err
	}

	matches := answer.Issues.Nodes
	if len(matches) == 0 {
		return linearCreated{}, false, nil
	}

	slices.SortFunc(matches, func(a, b linearCreated) int {
		return strings.Compare(a.CreatedAt+" "+a.Identifier, b.CreatedAt+" "+b.Identifier)
	})

	return matches[0], true, nil
}

func linearIssueIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, err := config.ParseLinearIssueSource(raw)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return fetchLinearIssue(ctx, source.Connection(), version, destDir)
}
