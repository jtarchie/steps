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

const linearIssueRefQuery = `query($id: String!) {
  issue(id: $id) { id identifier url team { id key name states(first: 250) { nodes { id name type } } } }
}`

const linearCommentMutation = `mutation($input: CommentCreateInput!) {
  commentCreate(input: $input) { success comment { id url } }
}`

const linearUpdateMutation = `mutation($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) { success issue { id identifier url state { name } } }
}`

// linearIssueRef is an issue as a put needs it: the uuid the mutations take, and the team its state and label names are looked up in.
type linearIssueRef struct {
	ID         string     `json:"id"`
	Identifier string     `json:"identifier"`
	URL        string     `json:"url"`
	Team       linearTeam `json:"team"`
}

// resolveLinearIssue finds which issue a put is about: params.issue when it says, else the version of the input params.from names, else the one input whose version carries an identifier, which is every linear-issues get's, so a put handed exactly one needs no params to say which.
func resolveLinearIssue(from, issue string, inputs PutInputs) (string, error) {
	if issue != "" {
		if !config.LinearIdentifierValid(issue) {
			return "", fmt.Errorf("params.issue: %q is not an issue identifier such as ENG-123", issue)
		}

		return issue, nil
	}

	if from != "" {
		identifier := versionField(inputs.Versions[from], "identifier")
		if !config.LinearIdentifierValid(identifier) {
			return "", fmt.Errorf("params.from: %q is not an input of this put whose version names an issue (inputs: %s)", from, strings.Join(inputs.Names, ", "))
		}

		return identifier, nil
	}

	var carrying []string

	for name, version := range inputs.Versions {
		if config.LinearIdentifierValid(versionField(version, "identifier")) {
			carrying = append(carrying, name)
		}
	}

	slices.Sort(carrying)

	switch len(carrying) {
	case 1:
		return versionField(inputs.Versions[carrying[0]], "identifier"), nil
	case 0:
		return "", errors.New("no input names an issue: give this put its linear-issues get as an input, or set params.issue")
	default:
		return "", fmt.Errorf("several inputs name an issue (%s): set params.from to the one this acts on", strings.Join(carrying, ", "))
	}
}

// issueRef reads the issue an identifier names, so a pipeline never handles uuids.
func (c *linearClient) issueRef(ctx context.Context, identifier string) (linearIssueRef, error) {
	var answer struct {
		Issue *linearIssueRef `json:"issue"`
	}

	err := c.query(ctx, linearIssueRefQuery, map[string]any{"id": identifier}, &answer)
	if err != nil {
		return linearIssueRef{}, err
	}

	if answer.Issue == nil {
		return linearIssueRef{}, errors.New("no Linear issue " + identifier)
	}

	return *answer.Issue, nil
}

// linearCommentOut posts one comment. Nothing dedupes: a retried put posts twice, as github-pr-comment does, so a pipeline that must not guards it before the put.
func linearCommentOut(ctx context.Context, raw, rawParams map[string]any, inputs PutInputs, srcDir string) (map[string]any, error) {
	source, err := config.ParseLinearPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseLinearCommentParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	identifier, err := resolveLinearIssue(params.From, params.Issue, inputs)
	if err != nil {
		return nil, err
	}

	body, err := readPostBody(srcDir, params.BodyFile)
	if err != nil {
		return nil, err
	}

	client, err := newLinearClient(ctx, source.Connection())
	if err != nil {
		return nil, err
	}

	issue, err := client.issueRef(ctx, identifier)
	if err != nil {
		return nil, err
	}

	var created struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}

	err = client.mutate(ctx, linearCommentMutation, map[string]any{"input": map[string]any{"issueId": issue.ID, "body": body}}, &created)
	if err != nil {
		return nil, err
	}

	if !created.CommentCreate.Success {
		return nil, fmt.Errorf("linear: commentCreate on %s did not succeed", identifier)
	}

	reportFacts(ctx, []events.Fact{{Name: "url", Value: created.CommentCreate.Comment.URL}})

	return map[string]any{"issue": identifier, "comment": created.CommentCreate.Comment.ID}, nil
}

// linearUpdateOut moves an issue and swaps its labels in one issueUpdate, so it lands whole or not at all. Naming a state the issue is already in, or removing a label it lacks, succeeds: that is the issue in the state asked for, which a replay finds.
func linearUpdateOut(ctx context.Context, raw, rawParams map[string]any, inputs PutInputs, _ string) (map[string]any, error) {
	source, err := config.ParseLinearPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseLinearUpdateParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	identifier, err := resolveLinearIssue(params.From, params.Issue, inputs)
	if err != nil {
		return nil, err
	}

	client, err := newLinearClient(ctx, source.Connection())
	if err != nil {
		return nil, err
	}

	issue, err := client.issueRef(ctx, identifier)
	if err != nil {
		return nil, err
	}

	input, err := client.updateInput(ctx, issue, params)
	if err != nil {
		return nil, err
	}

	var updated struct {
		IssueUpdate struct {
			Success bool `json:"success"`
			Issue   struct {
				URL string `json:"url"`
			} `json:"issue"`
		} `json:"issueUpdate"`
	}

	err = client.mutate(ctx, linearUpdateMutation, map[string]any{"id": issue.ID, "input": input}, &updated)
	if err != nil {
		return nil, err
	}

	if !updated.IssueUpdate.Success {
		return nil, fmt.Errorf("linear: issueUpdate on %s did not succeed", identifier)
	}

	reportFacts(ctx, []events.Fact{{Name: "url", Value: updated.IssueUpdate.Issue.URL}})

	return updateVersion(identifier, params), nil
}

// updateVersion records what the put asked, with a side left out when it did not ask for it.
func updateVersion(identifier string, params config.LinearUpdateParams) map[string]any {
	version := map[string]any{"issue": identifier}

	if params.State != "" {
		version["state"] = params.State
	}

	if len(params.AddLabels) > 0 {
		version["add_labels"] = strings.Join(params.AddLabels, ", ")
	}

	if len(params.RemoveLabels) > 0 {
		version["remove_labels"] = strings.Join(params.RemoveLabels, ", ")
	}

	return version
}

// updateInput resolves the names a put gave into the ids issueUpdate takes. Every name is resolved before anything is sent, so a typo in the last label leaves the issue untouched.
func (c *linearClient) updateInput(ctx context.Context, issue linearIssueRef, params config.LinearUpdateParams) (map[string]any, error) {
	input := map[string]any{}

	if params.State != "" {
		ids, err := stateIDs(issue.Team, []string{params.State})
		if err != nil {
			return nil, fmt.Errorf("params.state: team %s has no state named %q (it has: %s)", issue.Team.Key, params.State, stateNames(issue.Team))
		}

		input["stateId"] = ids[0]
	}

	if len(params.AddLabels) == 0 && len(params.RemoveLabels) == 0 {
		return input, nil
	}

	known, err := c.labelsOf(ctx, issue.Team.ID)
	if err != nil {
		return nil, err
	}

	for _, side := range []struct {
		field, key string
		names      []string
	}{
		{"params.add_labels", "addedLabelIds", params.AddLabels},
		{"params.remove_labels", "removedLabelIds", params.RemoveLabels},
	} {
		if len(side.names) == 0 {
			continue
		}

		input[side.key], err = labelIDs(side.field, known, side.names)
		if err != nil {
			return nil, err
		}
	}

	return input, nil
}
