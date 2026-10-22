package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

func githubCommentOut(ctx context.Context, raw, rawParams map[string]any, inputs PutInputs, srcDir string) (map[string]any, error) {
	source, err := config.ParseGitHubPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseGitHubPostParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	var answering commentTarget

	if params.InThread {
		answering, err = resolveComment(params.From, inputs)
	} else {
		var target pullRequestTarget

		target, err = resolvePullRequest(params, inputs)
		answering.number = target.number
	}

	if err != nil {
		return nil, err
	}

	body, err := readPostBody(srcDir, params.BodyFile)
	if err != nil {
		return nil, err
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return nil, err
	}

	path := "/repos/" + source.Repo + "/issues/" + answering.number + "/comments"

	// Only an inline comment has a thread; a conversation or an issue is one,
	// so its comment is answered there, as without in_thread.
	if answering.kind == config.GitHubCommentReview {
		path, err = threadReplies(ctx, client, source.Repo, answering)
		if err != nil {
			return nil, err
		}
	}

	return postExpecting(ctx, client, path, map[string]any{"body": body}, http.StatusCreated, answering.number)
}

// threadReplies is the route a reply under an inline comment posts to.
// GitHub's reference for it says the id "must be the ID of a top-level review
// comment, not a reply to that comment. Replies to replies are not
// supported." A command is as often written as a reply in a thread as at its
// start, so the comment is read first, and a reply stands in for the
// thread's first comment, which its in_reply_to_id names: GitHub keeps a
// review thread flat, every reply pointing at the top, so one read is enough.
func threadReplies(ctx context.Context, client *githubClient, repo string, comment commentTarget) (string, error) {
	var read struct {
		InReplyTo int64 `json:"in_reply_to_id"`
	}

	_, err := client.getJSON(ctx, commentPath(repo, comment.id, comment.kind), &read)
	if err != nil {
		return "", err
	}

	top := comment.id
	if read.InReplyTo != 0 {
		top = strconv.FormatInt(read.InReplyTo, 10)
	}

	return "/repos/" + repo + "/pulls/" + comment.number + "/comments/" + url.PathEscape(top) + "/replies", nil
}

func githubReviewOut(ctx context.Context, raw, rawParams map[string]any, inputs PutInputs, srcDir string) (map[string]any, error) {
	source, err := config.ParseGitHubPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseGitHubReviewParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	target, err := resolvePullRequest(params.Post(), inputs)
	if err != nil {
		return nil, err
	}

	// Read and checked before anything is sent: a pending put deletes the
	// last draft first, and a malformed comment found after that would leave
	// no draft at all.
	payload, err := reviewPayload(srcDir, params)
	if err != nil {
		return nil, err
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return nil, err
	}

	reviews := "/repos/" + source.Repo + "/pulls/" + target.number + "/reviews"

	// On the commit that was fetched, when the input says which: a review
	// lands on a commit, and the pull request may have moved since.
	if target.sha != "" {
		payload["commit_id"] = target.sha
	}

	if params.Event == "pending" {
		err = discardOwnPendingReview(ctx, client, reviews)
		if err != nil {
			return nil, err
		}
	} else {
		payload["event"] = strings.ToUpper(cmpOr(params.Event, "comment"))
	}

	return postExpecting(ctx, client, reviews, payload, http.StatusOK, target.number)
}

// reviewPayload is a review's body and inline comments, read from the put's
// inputs.
func reviewPayload(srcDir string, params config.GitHubReviewParams) (map[string]any, error) {
	body, err := readPostBody(srcDir, params.BodyFile)
	if err != nil {
		return nil, err
	}

	comments, err := readReviewComments(srcDir, params.CommentsFile)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{"body": body}

	if len(comments) > 0 {
		payload["comments"] = comments
	}

	return payload, nil
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

// discardOwnPendingReview deletes the token user's own draft review, which
// GitHub allows one of per pull request — so a new push's draft replaces the
// last one rather than failing beside it. Anybody else's draft is theirs.
func discardOwnPendingReview(ctx context.Context, client *githubClient, reviews string) error {
	login, err := client.login(ctx)
	if err != nil {
		return err
	}

	for page := 1; page <= githubPageLimit; page++ {
		var listed []struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
			User  struct {
				Login string `json:"login"`
			} `json:"user"`
		}

		_, err := client.getJSON(ctx, reviews+"?per_page=100&page="+strconv.Itoa(page), &listed)
		if err != nil {
			return err
		}

		for _, review := range listed {
			if review.State != "PENDING" || !strings.EqualFold(review.User.Login, login) {
				continue
			}

			path := reviews + "/" + strconv.FormatInt(review.ID, 10)

			reply, err := client.send(ctx, http.MethodDelete, client.api+path, githubJSON, nil)
			if err != nil {
				return err
			}

			if reply.status != http.StatusOK {
				return githubFailure(http.MethodDelete, path, reply)
			}
		}

		if len(listed) < githubPageSize {
			return nil
		}
	}

	return nil
}

func githubReactionOut(ctx context.Context, raw, rawParams map[string]any, inputs PutInputs, _ string) (map[string]any, error) {
	source, err := config.ParseGitHubPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseGitHubReactionParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	target, err := resolveComment(params.From, inputs)
	if err != nil {
		return nil, err
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return nil, err
	}

	reactions := commentPath(source.Repo, target.id, target.kind) + "/reactions"
	version := map[string]any{"comment": target.id}

	// Add first: a swap that failed halfway leaves two marks rather than
	// none, and none reads as "nobody picked this up".
	if params.Add != "" {
		err = addReaction(ctx, client, reactions, params.Add)
		if err != nil {
			return nil, err
		}

		version["add"] = params.Add
	}

	if params.Remove != "" {
		err = removeOwnReaction(ctx, client, reactions, params.Remove)
		if err != nil {
			return nil, err
		}

		version["remove"] = params.Remove
	}

	return version, nil
}

// addReaction puts content on a comment. 200 is GitHub saying this user's
// reaction is already there, which a replay always finds — the world as
// asked, like Slack's already_reacted.
func addReaction(ctx context.Context, client *githubClient, path, content string) error {
	reply, err := client.send(ctx, http.MethodPost, client.api+path, githubJSON, map[string]any{"content": content})
	if err != nil {
		return err
	}

	if reply.status != http.StatusCreated && reply.status != http.StatusOK {
		return githubFailure(http.MethodPost, path, reply)
	}

	return nil
}

// removeOwnReaction takes the token user's own content reaction off a
// comment. It is found by user because the delete route takes a reaction's
// id, and a content alone names everybody's. Not there, or gone by the time
// the delete lands, is the world as asked.
func removeOwnReaction(ctx context.Context, client *githubClient, path, content string) error {
	login, err := client.login(ctx)
	if err != nil {
		return err
	}

	id, found, err := findOwnReaction(ctx, client, path, content, login)
	if err != nil || !found {
		return err
	}

	target := path + "/" + strconv.FormatInt(id, 10)

	reply, err := client.send(ctx, http.MethodDelete, client.api+target, githubJSON, nil)
	if err != nil {
		return err
	}

	if reply.status != http.StatusNoContent && reply.status != http.StatusNotFound {
		return githubFailure(http.MethodDelete, target, reply)
	}

	return nil
}

type commentTarget struct {
	id     string
	kind   string
	number string
}

// resolveComment finds which comment a reaction marks or a reply answers:
// the version of the input from names, else the one input whose version is a
// github-comments get's — the only versions that carry a kind.
func resolveComment(from string, inputs PutInputs) (commentTarget, error) {
	isComment := func(version map[string]any) bool {
		return versionField(version, "id") != "" && versionField(version, "kind") != ""
	}

	var version map[string]any

	if from != "" {
		version = inputs.Versions[from]
		if !isComment(version) {
			return commentTarget{}, fmt.Errorf("params.from: %q is not an input of this put whose version names a comment (inputs: %s)",
				from, strings.Join(inputs.Names, ", "))
		}
	} else {
		var carrying []string

		for name, candidate := range inputs.Versions {
			if isComment(candidate) {
				carrying = append(carrying, name)
			}
		}

		slices.Sort(carrying)

		switch len(carrying) {
		case 1:
			version = inputs.Versions[carrying[0]]
		case 0:
			return commentTarget{}, errors.New("no input names a comment: give this put its github-comments get as an input")
		default:
			return commentTarget{}, fmt.Errorf("several inputs name a comment (%s): set params.from to the one this put is about", strings.Join(carrying, ", "))
		}
	}

	target := commentTarget{id: versionField(version, "id"), kind: versionField(version, "kind"), number: versionField(version, "number")}

	if !slices.Contains([]string{config.GitHubCommentConversation, config.GitHubCommentReview, config.GitHubCommentIssue}, target.kind) {
		return commentTarget{}, fmt.Errorf("the comment's version names kind %q, which is no comment kind GitHub has", target.kind)
	}

	return target, nil
}

func postExpecting(ctx context.Context, client *githubClient, path string, payload map[string]any, want int, number string) (map[string]any, error) {
	reply, err := client.send(ctx, http.MethodPost, client.api+path, githubJSON, payload)
	if err != nil {
		return nil, err
	}

	if reply.status != want {
		return nil, githubFailure(http.MethodPost, path, reply)
	}

	var created struct {
		ID int64 `json:"id"`
	}

	err = json.Unmarshal(reply.body, &created)
	if err != nil {
		return nil, fmt.Errorf("github: POST %s: %w", path, err)
	}

	return map[string]any{"id": strconv.FormatInt(created.ID, 10), "number": number}, nil
}

type pullRequestTarget struct {
	number string
	sha    string
}

// resolvePullRequest finds which pull request a put is about: params.number
// when it says, else the version of the input params.from names, else the
// one input whose version carries a number — which is every github-prs and
// github-comments get, so a put handed exactly one of them needs no params
// to say which.
func resolvePullRequest(params config.GitHubPostParams, inputs PutInputs) (pullRequestTarget, error) {
	if params.Number != "" {
		return pullRequestTarget{number: params.Number}, nil
	}

	if params.From != "" {
		version, ok := inputs.Versions[params.From]
		if !ok || versionField(version, "number") == "" {
			return pullRequestTarget{}, fmt.Errorf("params.from: %q is not an input of this put whose version names a pull request (inputs: %s)",
				params.From, strings.Join(inputs.Names, ", "))
		}

		return targetOf(version), nil
	}

	var carrying []string

	for name, version := range inputs.Versions {
		if versionField(version, "number") != "" {
			carrying = append(carrying, name)
		}
	}

	slices.Sort(carrying)

	switch len(carrying) {
	case 1:
		return targetOf(inputs.Versions[carrying[0]]), nil
	case 0:
		return pullRequestTarget{}, errors.New("no input names a pull request: give this put its github-prs or github-comments get as an input, or set params.number")
	default:
		return pullRequestTarget{}, fmt.Errorf("several inputs name a pull request (%s): set params.from to the one this posts on", strings.Join(carrying, ", "))
	}
}

// targetOf reads a version as a pull request. A comment's version names the
// pull request it sits on and no commit, which leaves a review on the head.
func targetOf(version map[string]any) pullRequestTarget {
	target := pullRequestTarget{number: versionField(version, "number")}

	if _, isComment := version["kind"]; !isComment {
		target.sha = versionField(version, "sha")
	}

	return target
}

// readPostBody reads the text to post from inside the put's own tree. An
// empty one is refused: posting nothing is how a step that wrote nothing
// would otherwise look like one that succeeded.
func readPostBody(srcDir, name string) (string, error) {
	root, err := os.OpenRoot(srcDir)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	defer func() { _ = root.Close() }()

	body, err := root.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("params.body_file: %w — is the step that writes it one of this put's inputs?", err)
	}

	if strings.TrimSpace(string(body)) == "" {
		return "", fmt.Errorf("params.body_file: %s is empty, so there is nothing to post", name)
	}

	return string(body), nil
}

// reviewComment is one comment on a line of a pull request's diff, as
// GitHub's create-review call takes it. Line is the line in the file, not a
// position in the diff; side RIGHT (the default) is the new file.
type reviewComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	Side      string `json:"side,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

// readReviewComments reads params.comments_file, when set. Unknown fields are
// refused rather than dropped: a model that writes `file:` for `path:` should
// fail here with the field named, not post a review missing the comment.
// Whether each line is IN the diff is GitHub's to say, since only it knows
// which lines a review may anchor to; it refuses the whole review if one
// is not.
func readReviewComments(srcDir, name string) ([]reviewComment, error) {
	if name == "" {
		return nil, nil
	}

	root, err := os.OpenRoot(srcDir)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	defer func() { _ = root.Close() }()

	raw, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("params.comments_file: %w — is the step that writes it one of this put's inputs?", err)
	}

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()

	var comments []reviewComment

	err = decoder.Decode(&comments)
	if err != nil {
		return nil, fmt.Errorf("params.comments_file: %s is not a JSON array of {path, line, body}: %w", name, err)
	}

	for i := range comments {
		err := checkReviewComment(&comments[i])
		if err != nil {
			return nil, fmt.Errorf("params.comments_file: comment %d (%s:%d): %w", i+1, comments[i].Path, comments[i].Line, err)
		}
	}

	return comments, nil
}

func checkReviewComment(comment *reviewComment) error {
	err := checkReviewAnchor(*comment)
	if err != nil {
		return err
	}

	if strings.TrimSpace(comment.Body) == "" {
		return errors.New("body is empty, so there is nothing to say on that line")
	}

	// A range sits on one side; GitHub wants both ends told.
	if comment.StartLine > 0 && comment.StartSide == "" {
		comment.StartSide = comment.Side
	}

	return nil
}

// checkReviewAnchor checks where a comment sits, short of whether GitHub will
// find that line in the diff.
func checkReviewAnchor(comment reviewComment) error {
	switch {
	case comment.Path == "" || strings.HasPrefix(comment.Path, "/"):
		return errors.New("path must be the file's path in the repository, relative to its root")
	case comment.Line < 1:
		return errors.New("line must be a line number in the file, from 1")
	case comment.StartLine < 0 || (comment.StartLine > 0 && comment.StartLine >= comment.Line):
		return errors.New("start_line, for a range, must come before line")
	case comment.Side != "" && comment.Side != "LEFT" && comment.Side != "RIGHT":
		return fmt.Errorf("side %q is not LEFT (the old file) or RIGHT (the new one)", comment.Side)
	}

	return nil
}
