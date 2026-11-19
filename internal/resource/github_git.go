package resource

// `checkout: git`: a pull request laid down as a git repository rather than
// an archive, for a step that wants git's own answers — `git merge-base`,
// `git diff origin/<base>...HEAD`, a commit to push.
//
// The git binary does the cloning, under the built-in git type's hermetic
// rules (internal/config/resource_types/git.yml): no operator config, no
// prompt, no ssh, and the credential only from source.token_env. The token
// travels as an Authorization header scoped to the one clone URL, set
// through GIT_CONFIG_* in the child's environment, so it is in no argv and
// nothing under .git: a later step's `git fetch` there is unauthenticated.
//
// The history is as shallow as the compare GitHub already answered allows:
// ahead_by+1 commits of the head and behind_by+1 of the base both end at the
// merge base. A base branch that moved in between leaves it out, and that
// one case deepens both to full history rather than guessing a depth again.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// gitClone is what one clone needs: where from, as whom, and the pull request's place in the base branch's history.
type gitClone struct {
	url, token    string
	number, sha   string
	base          string
	ahead, behind int
	dir           string
	// image and runner are the type's image: and the container the clone's commands run in; nil runs git on this machine.
	image  string
	runner shell.Runner
}

type gitImageKey struct{}

// withGitImage has checkout: git's clone run in the type's image (#263), as its other built-ins' commands do.
func withGitImage(ctx context.Context, rt config.ResourceType, resource config.Resource) context.Context {
	if rt.Image == "" {
		return ctx
	}

	return context.WithValue(ctx, gitImageKey{}, runnerSpec(rt, resource.Env))
}

// inImage is c with its container started when ctx asked for one; the returned func gives it back.
func (c gitClone) inImage(ctx context.Context) (gitClone, func(), error) {
	spec, ok := ctx.Value(gitImageKey{}).(shell.RunnerSpec)
	if !ok {
		return c, func() {}, nil
	}

	spec.Cwd, spec.Workdir, spec.FetchAll, spec.EnvValues = c.dir, shell.GetDir, true, c.gitEnv()

	runner, err := newRunner(ctx, spec)
	if err != nil {
		return c, nil, fmt.Errorf("checkout: git in image %q: %w", spec.Image, err)
	}

	c.image, c.runner = spec.Image, runner

	return c, func() { shell.CloseRunner(runner, "checkout: git") }, nil
}

// checkoutGit makes c.dir a repository at c.sha, detached, with origin/<base>
// beside it and the merge base of the two in its history.
func checkoutGit(ctx context.Context, c gitClone) error {
	err := checkCloneURL(c.url)
	if err != nil {
		return err
	}

	// The commit reaches git's argv, where one spelled as an option would be read as one.
	if !commitID.MatchString(c.sha) {
		return fmt.Errorf("github: %q is not a commit id", c.sha)
	}

	ctx, cancel := context.WithTimeout(ctx, githubFetchTimeout)
	defer cancel()

	c, release, err := c.inImage(ctx)
	if err != nil {
		return err
	}

	defer release()

	err = c.fetch(ctx)
	if err != nil {
		return err
	}

	err = c.git(ctx, "checkout", "--quiet", "--detach", c.sha)
	if err != nil {
		return fmt.Errorf("pull request #%s: commit %s is not on it any more: %w", c.number, c.sha, err)
	}

	if c.git(ctx, "merge-base", "HEAD", "origin/"+c.base) == nil {
		return nil
	}

	err = c.git(ctx, append(c.fetchArgs(), "--unshallow", c.pullRef(), c.baseRef())...)
	if err != nil {
		return err
	}

	err = c.git(ctx, "merge-base", "HEAD", "origin/"+c.base)
	if err != nil {
		return fmt.Errorf("pull request #%s shares no history with %s: %w", c.number, c.base, err)
	}

	return nil
}

// fetch makes the repository and fetches the head and the base, each as deep as the compare said reaches the merge base.
func (c gitClone) fetch(ctx context.Context) error {
	for _, args := range [][]string{
		{"init", "--quiet", "--template=", "--initial-branch=pull/" + c.number},
		{"remote", "add", "origin", c.url},
	} {
		err := c.git(ctx, args...)
		if err != nil {
			return err
		}
	}

	// By id first: the version's commit, even when the pull request has moved on since. A server that fetches only advertised refs refuses it, and the pull request's own ref is what such a server does serve — a fork's commit included, since GitHub mirrors it there.
	err := c.git(ctx, append(c.fetchArgs(), depth(c.ahead), c.sha)...)
	if err != nil {
		err = c.git(ctx, append(c.fetchArgs(), c.pullRef())...)
		if err != nil {
			return err
		}
	}

	return c.git(ctx, append(c.fetchArgs(), depth(c.behind), c.baseRef())...)
}

func (c gitClone) fetchArgs() []string {
	return []string{"fetch", "--quiet", "--no-tags", "--no-auto-gc", "--no-write-fetch-head", "origin"}
}

func (c gitClone) pullRef() string {
	return "+refs/pull/" + c.number + "/head:refs/remotes/origin/pull/" + c.number
}

func (c gitClone) baseRef() string {
	return "+refs/heads/" + c.base + ":refs/remotes/origin/" + c.base
}

// commitID is a SHA-1 or SHA-256 object id.
var commitID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`) //nolint:gochecknoglobals // compiled once, read-only

func depth(commits int) string { return "--depth=" + strconv.Itoa(max(commits, 0)+1) }

// checkCloneURL holds the clone URL GitHub answered to source.endpoint's rule: https, or http to this machine only, since the token goes with it.
func checkCloneURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("github: clone URL %q is not an http(s) URL", raw)
	}

	if parsed.Scheme == "https" {
		return nil
	}

	host := parsed.Hostname()
	ip := net.ParseIP(host)

	if parsed.Scheme == "http" && (strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())) {
		return nil
	}

	return fmt.Errorf("github: clone URL %q is not https, and the token would travel in cleartext", raw)
}

// git runs one git command in the clone, its stderr in the error.
func (c gitClone) git(ctx context.Context, args ...string) error {
	if c.runner != nil {
		return c.gitInImage(ctx, args)
	}

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // the arguments are fixed flags, refs built from a decimal number and GitHub's base branch, and a commit held to commitID
	cmd.Dir = c.dir
	cmd.Env = c.environ()

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("checkout: git needs the git binary on this machine's PATH")
	}

	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

func (c gitClone) gitInImage(ctx context.Context, args []string) error {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}

	res, err := c.runner.Exec(ctx, shell.Command{Script: "git " + strings.Join(quoted, " "), Capture: true, ExitAsData: true})

	switch {
	case err != nil:
		return fmt.Errorf("git %s in image %q: %w", strings.Join(args, " "), c.image, err)
	case res.Code == 127:
		return fmt.Errorf("checkout: git needs the git binary in image %q", c.image)
	case res.Code != 0:
		return fmt.Errorf("git %s: exit status %d: %s", strings.Join(args, " "), res.Code, strings.TrimSpace(res.Stderr))
	}

	return nil
}

// environ is the host baseline with gitEnv on.
func (c gitClone) environ() []string {
	env := shell.HostEnv()

	for name, value := range c.gitEnv() {
		env = append(env, name+"="+value)
	}

	return env
}

// gitEnv turns everything of the operator's git off and the one header on.
func (c gitClone) gitEnv() map[string]string {
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+c.token))
	config := [][2]string{
		// Empty clears any helper a config could still name.
		{"credential.helper", ""},
		{"http." + c.url + ".extraHeader", header},
		// A redirect would carry the header to wherever it points.
		{"http.followRedirects", "false"},
		{"core.hooksPath", os.DevNull},
		{"http.lowSpeedLimit", "1000"},
		{"http.lowSpeedTime", "60"},
		{"advice.detachedHead", "false"},
	}

	env := map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
		"GIT_ALLOW_PROTOCOL": "http:https", "GIT_CONFIG_COUNT": strconv.Itoa(len(config)),
	}

	for i, kv := range config {
		env[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)], env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)] = kv[0], kv[1]
	}

	return env
}

// writeGetFiles writes a get's metadata files beside the tree and, in a repository, keeps them out of it.
func writeGetFiles(destDir string, files map[string][]byte, checkout config.GitHubCheckout) error {
	err := writeMetadata(destDir, files)
	if err != nil || checkout != config.GitHubCheckoutGit {
		return err
	}

	return excludeMetadata(destDir, slices.Sorted(maps.Keys(files)))
}

// excludeMetadata adds names to the clone's ignored files, keeping the files a get writes beside the tree out of `git status` and `git add -A`: they describe the pull request and are not part of it.
func excludeMetadata(dir string, names []string) error {
	info := filepath.Join(dir, ".git", "info")

	err := os.MkdirAll(info, 0o755) //nolint:gosec // readable as git leaves its own .git/info, for a containerized step running as another user
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, "/"+name)
	}

	file, err := os.OpenFile(filepath.Join(info, "exclude"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644) //nolint:gosec // a clone's own files are readable, as git leaves them
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	_, err = file.WriteString(strings.Join(lines, "\n") + "\n")
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("%w", err)
	}

	return wrapIfErr(file.Close())
}
