package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// sandboxPolicy is what a fenced host command may touch. Paths are symlink-resolved, because an OS sandbox matches the real path: a rule naming /var never matches a write to /private/var.
type sandboxPolicy struct {
	writable []string
	denyRead []string
	network  bool
}

// sandboxer is the seam an OS sandbox fills. sandbox_darwin.go is seatbelt (sandbox-exec); every other OS refuses rather than running the command unfenced.
type sandboxer interface {
	command(policy sandboxPolicy, argv []string) ([]string, error)
}

// credentialPaths are the home-relative paths a fenced command may not read. Reads are otherwise open: a host command needs its toolchain wherever the operator installed it, and enumerating that is what makes deny-default reads unusable.
var credentialPaths = []string{ //nolint:gochecknoglobals // static, read-only list
	".ssh", ".aws", ".config/gh", ".config/gcloud", ".kube", ".gnupg", ".netrc", ".docker/config.json", "Library/Keychains",
}

// hostFence is one sandboxed HostRunner: its policy and the private TMPDIR it owns, which Close removes.
type hostFence struct {
	box    sandboxer
	policy sandboxPolicy
	tmp    string
	probed sync.Once
	// unusable is the probe's answer, kept so every later command of the step fails the same way.
	unusable error
}

// newHostFence fences commands that may write only under cwd and a fresh private TMPDIR. The shared temp dir is not writable: it is where every other step's workspace lives.
func newHostFence(box sandboxer, cwd string, network bool) (*hostFence, error) {
	tmp, err := os.MkdirTemp("", "steps-sandbox-")
	if err != nil {
		return nil, fmt.Errorf("sandbox: private TMPDIR: %w", err)
	}

	fence := &hostFence{box: box, tmp: tmp, policy: sandboxPolicy{network: network}}

	err = fence.build(cwd)
	if err != nil {
		return nil, errors.Join(err, fence.close())
	}

	return fence, nil
}

func (f *hostFence) build(cwd string) error {
	for _, dir := range []string{f.tmp, cwd} {
		if dir == "" {
			continue // a resource check runs with no tree
		}

		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return fmt.Errorf("sandbox: resolve %q: %w", dir, err)
		}

		f.policy.writable = append(f.policy.writable, resolved)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("sandbox: cannot name the credential paths to deny: %w", err)
	}

	f.policy.denyRead = denyReadPaths(home)

	return nil
}

// denyReadPaths names each credential path both as spelled and as resolved, so a dotfile manager symlinking ~/.ssh elsewhere does not slip it past a rule matching the real path.
func denyReadPaths(home string) []string {
	var out []string

	for _, rel := range credentialPaths {
		path := filepath.Join(home, rel)
		out = append(out, path)

		resolved, err := filepath.EvalSymlinks(path)
		if err == nil && resolved != path {
			out = append(out, resolved)
		}
	}

	return out
}

// ready probes the fence before the step's first command, so a sandbox that cannot apply (an unsupported OS, or steps itself inside a sandbox that forbids nesting) is an infrastructure error rather than every command exiting nonzero, which would read as the step saying no. At the first command rather than in NewRunner, which has no context, and so a cache hit never pays for it.
func (f *hostFence) ready(ctx context.Context) error {
	f.probed.Do(func() { f.unusable = f.probe(ctx) })

	return f.unusable
}

func (f *hostFence) probe(ctx context.Context) error {
	argv, err := f.box.command(f.policy, []string{"sh", "-c", ":"})
	if err != nil {
		return err
	}

	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // argv is the fixed sandbox wrapper around a no-op
	if err != nil {
		// %v, never %w: an *exec.ExitError in the chain makes IsExitError call this the step saying no.
		return fmt.Errorf("sandbox: cannot apply the host sandbox here: %v: %s", err, out) //nolint:errorlint // see above
	}

	return nil
}

// env points every temp-dir variable at the private TMPDIR. os/exec keeps the last duplicate, so appending overrides the allowlisted ones.
func (f *hostFence) env(base []string) []string {
	return append(base, "TMPDIR="+f.tmp, "TMP="+f.tmp, "TEMP="+f.tmp)
}

func (f *hostFence) close() error {
	err := os.RemoveAll(f.tmp)
	if err != nil {
		return fmt.Errorf("sandbox: remove private TMPDIR: %w", err)
	}

	return nil
}
