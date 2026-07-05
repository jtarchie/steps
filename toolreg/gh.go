package toolreg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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

func registerGH(r *Registry) {
	r.Register("gh.pr_diff", "Fetch a pull request's diff, title, and description via the gh CLI",
		func(ctx context.Context, args map[string]any) (map[string]any, error) {
			pr, err := str(args, "pr")
			if err != nil {
				return nil, err
			}
			common := []string{}
			if repo, _ := args["repo"].(string); repo != "" {
				common = append(common, "--repo", repo)
			}
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
		})

	r.Register("gh.post_review", "Post a pull request review (approve | comment | request_changes) via the gh CLI",
		func(ctx context.Context, args map[string]any) (map[string]any, error) {
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
			ghArgs := []string{"pr", "review", pr, flag, "--body", body}
			if repo, _ := args["repo"].(string); repo != "" {
				ghArgs = append(ghArgs, "--repo", repo)
			}
			_, err = runGH(ctx, ghArgs...)
			if err != nil {
				return nil, err
			}
			return map[string]any{"posted": true, "event": event}, nil
		})
}
