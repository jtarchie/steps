package toolreg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// The gh action pack shells out to the GitHub CLI (`gh`), which carries its
// own auth. Registered unconditionally; runtime errors are precise when gh
// is missing or unauthenticated.

func runGH(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", strings.Join(args[:min(2, len(args))], " "), msg)
	}
	return out.String(), nil
}

// repoArgs appends `--repo owner/repo` when a repo is given — the common
// override for gh's PR subcommands.
func repoArgs(args map[string]any) []string {
	if repo, _ := args["repo"].(string); repo != "" {
		return []string{"--repo", repo}
	}
	return nil
}

// strSlice reads an optional []string argument. It accepts a JS array
// (rendered as []any of strings) and tolerates absence (nil).
func strSlice(args map[string]any, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an array of strings, got %T", key, raw)
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q[%d] must be a string, got %T", key, i, it)
		}
		out = append(out, s)
	}
	return out, nil
}

func registerGH(r *Registry) {
	r.Register("gh.pr_diff", "Fetch a pull request's diff, title, and description via the gh CLI; when a non-empty diff arg is supplied it is echoed back without calling gh (offline/fixture passthrough)", ghPRDiff)
	r.Register("gh.post_review", "Post a pull request review (approve | comment | request_changes) via the gh CLI", ghPostReview)
	r.Register("gh.comment", "Post a plain comment on a pull request via the gh CLI; returns {posted, url}", ghComment)
	r.Register("gh.pr_meta", "Fetch pull request metadata (draft, author, headSha, labels, change counts) via the gh CLI", ghPRMeta)
	r.Register("gh.review_comment", "Post an inline pull request review comment at a file:line via the GitHub API; returns {posted, url}", ghReviewComment)
	r.Register("gh.label", "Add and/or remove labels on a pull request via the gh CLI", ghLabel)
	r.Register("gh.status", "Set a commit status/check on a SHA via the GitHub API; state is success | failure | pending | error", ghStatus)
}

// ghPRDiff fetches a PR's diff + metadata. PASSTHROUGH: a non-empty `diff` arg
// is echoed back (with the given title/description) WITHOUT calling gh — the
// seam that lets one machine run on a fixture diff offline and on a live PR via
// webhook with no branching.
func ghPRDiff(ctx context.Context, args map[string]any) (map[string]any, error) {
	if diff, _ := args["diff"].(string); diff != "" {
		title, _ := args["title"].(string)
		desc, _ := args["description"].(string)
		return map[string]any{"diff": diff, "title": title, "description": desc}, nil
	}
	pr, err := str(args, "pr")
	if err != nil {
		return nil, err
	}
	common := repoArgs(args)
	diff, err := runGH(ctx, append([]string{"pr", "diff", pr}, common...)...)
	if err != nil {
		return nil, err
	}
	meta, err := runGH(ctx, append([]string{"pr", "view", pr, "--json", "title,body"}, common...)...)
	if err != nil {
		return nil, err
	}
	var view struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	err = json.Unmarshal([]byte(meta), &view)
	if err != nil {
		return nil, fmt.Errorf("parsing gh pr view output: %w", err)
	}
	return map[string]any{"diff": diff, "title": view.Title, "description": view.Body}, nil
}

func ghPostReview(ctx context.Context, args map[string]any) (map[string]any, error) {
	pr, err := str(args, "pr")
	if err != nil {
		return nil, err
	}
	body, err := str(args, "body")
	if err != nil {
		return nil, err
	}
	event, _ := args["event"].(string)
	var flag string
	switch event {
	case "approve":
		flag = "--approve"
	case "request_changes":
		flag = "--request-changes"
	case "", "comment":
		flag = "--comment"
	default:
		return nil, fmt.Errorf("event must be approve, comment, or request_changes, got %q", event)
	}
	ghArgs := append([]string{"pr", "review", pr, flag, "--body", body}, repoArgs(args)...)
	_, err = runGH(ctx, ghArgs...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"posted": true, "event": event}, nil
}

// ghComment leaves a plain PR comment (`gh pr comment`). Distinct from
// gh.post_review — a review cannot be left on your own PR, a comment always
// can. gh prints the new comment's URL to stdout.
func ghComment(ctx context.Context, args map[string]any) (map[string]any, error) {
	pr, err := str(args, "pr")
	if err != nil {
		return nil, err
	}
	body, err := str(args, "body")
	if err != nil {
		return nil, err
	}
	ghArgs := append([]string{"pr", "comment", pr, "--body", body}, repoArgs(args)...)
	out, err := runGH(ctx, ghArgs...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"posted": true, "url": strings.TrimSpace(out)}, nil
}

// ghPRMeta fetches a PR's routing signals (draft?, author/bot, head SHA,
// labels, change counts) so a machine can skip drafts/bots before spending
// tokens and can supply commit_id/path/line to gh.review_comment.
func ghPRMeta(ctx context.Context, args map[string]any) (map[string]any, error) {
	pr, err := str(args, "pr")
	if err != nil {
		return nil, err
	}
	fields := "number,title,body,isDraft,author,headRefName,headRefOid,baseRefName,labels,additions,deletions,changedFiles"
	ghArgs := append([]string{"pr", "view", pr, "--json", fields}, repoArgs(args)...)
	out, err := runGH(ctx, ghArgs...)
	if err != nil {
		return nil, err
	}
	var v struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		IsDraft bool   `json:"isDraft"`
		Author  struct {
			Login string `json:"login"`
			IsBot bool   `json:"is_bot"`
		} `json:"author"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		BaseRefName string `json:"baseRefName"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
		ChangedFiles int `json:"changedFiles"`
	}
	err = json.Unmarshal([]byte(out), &v)
	if err != nil {
		return nil, fmt.Errorf("parsing gh pr view output: %w", err)
	}
	labels := make([]any, 0, len(v.Labels))
	for _, l := range v.Labels {
		labels = append(labels, l.Name)
	}
	return map[string]any{
		"number":       v.Number,
		"title":        v.Title,
		"body":         v.Body,
		"draft":        v.IsDraft,
		"author":       v.Author.Login,
		"isBot":        v.Author.IsBot,
		"headSha":      v.HeadRefOid,
		"headRef":      v.HeadRefName,
		"baseRef":      v.BaseRefName,
		"labels":       labels,
		"additions":    v.Additions,
		"deletions":    v.Deletions,
		"changedFiles": v.ChangedFiles,
	}, nil
}

// ghReviewComment posts a single inline review comment at file:line via the
// reviews API. commit_id is the PR head SHA (from gh.pr_meta.headSha).
func ghReviewComment(ctx context.Context, args map[string]any) (map[string]any, error) {
	repo, err := str(args, "repo")
	if err != nil {
		return nil, err
	}
	pr, err := str(args, "pr")
	if err != nil {
		return nil, err
	}
	commit, err := str(args, "commit_id")
	if err != nil {
		return nil, err
	}
	path, err := str(args, "path")
	if err != nil {
		return nil, err
	}
	body, err := str(args, "body")
	if err != nil {
		return nil, err
	}
	line, err := toInt(args["line"])
	if err != nil {
		return nil, fmt.Errorf("argument \"line\": %w", err)
	}
	side, _ := args["side"].(string)
	if side == "" {
		side = "RIGHT"
	}
	ghArgs := []string{
		"api", fmt.Sprintf("repos/%s/pulls/%s/comments", repo, pr),
		"-f", "body=" + body,
		"-f", "commit_id=" + commit,
		"-f", "path=" + path,
		"-F", "line=" + strconv.Itoa(line),
		"-f", "side=" + side,
	}
	out, err := runGH(ctx, ghArgs...)
	if err != nil {
		return nil, err
	}
	var resp struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal([]byte(out), &resp)
	return map[string]any{"posted": true, "url": resp.HTMLURL}, nil
}

// ghLabel adds and/or removes labels on a PR (`gh pr edit`).
func ghLabel(ctx context.Context, args map[string]any) (map[string]any, error) {
	pr, err := str(args, "pr")
	if err != nil {
		return nil, err
	}
	add, err := strSlice(args, "add")
	if err != nil {
		return nil, err
	}
	remove, err := strSlice(args, "remove")
	if err != nil {
		return nil, err
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil, errors.New("gh.label needs at least one of add or remove")
	}
	ghArgs := []string{"pr", "edit", pr}
	for _, l := range add {
		ghArgs = append(ghArgs, "--add-label", l)
	}
	for _, l := range remove {
		ghArgs = append(ghArgs, "--remove-label", l)
	}
	ghArgs = append(ghArgs, repoArgs(args)...)
	_, err = runGH(ctx, ghArgs...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"labeled": true}, nil
}

// ghStatus sets a commit status/check on a SHA (e.g. the PR head) so the review
// verdict surfaces as a GitHub check.
func ghStatus(ctx context.Context, args map[string]any) (map[string]any, error) {
	repo, err := str(args, "repo")
	if err != nil {
		return nil, err
	}
	sha, err := str(args, "sha")
	if err != nil {
		return nil, err
	}
	state, err := str(args, "state")
	if err != nil {
		return nil, err
	}
	switch state {
	case "success", "failure", "pending", "error":
	default:
		return nil, fmt.Errorf("state must be success, failure, pending, or error, got %q", state)
	}
	checkName, err := str(args, "context")
	if err != nil {
		return nil, err
	}
	ghArgs := []string{
		"api", fmt.Sprintf("repos/%s/statuses/%s", repo, sha),
		"-f", "state=" + state,
		"-f", "context=" + checkName,
	}
	if desc, _ := args["description"].(string); desc != "" {
		ghArgs = append(ghArgs, "-f", "description="+desc)
	}
	if target, _ := args["target_url"].(string); target != "" {
		ghArgs = append(ghArgs, "-f", "target_url="+target)
	}
	_, err = runGH(ctx, ghArgs...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"posted": true}, nil
}
