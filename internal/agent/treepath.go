package agent

// Keeping a model's file paths inside the step's working directory when that directory is in a container.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// resolve confines rel to the container's tree. The lexical half is the same string work the host does and stays here; the symlink half has to be asked of the container, because a link resolves against ITS root — where /etc/passwd is the image's, not this machine's.
func (c containerTree) resolve(ctx context.Context, rel string) (string, error) {
	resolved, err := lexicalResolve(c.dir, rel)
	if err != nil {
		return "", err
	}

	return resolved, c.rejectLinkEscape(ctx, resolved, rel)
}

// resolveWrite closes the gap resolve leaves for a file that does not exist yet: a missing leaf has no link to follow, so a parent directory planted as a symlink out of the tree would never be looked at. The closest ancestor that does exist is checked instead, and only then are the parents created.
func (c containerTree) resolveWrite(ctx context.Context, rel string) (string, error) {
	resolved, err := lexicalResolve(c.dir, rel)
	if err != nil {
		return "", err
	}

	exists, err := c.pathExists(ctx, resolved)
	if err != nil {
		return "", err
	}

	if exists {
		return resolved, c.rejectLinkEscape(ctx, resolved, rel)
	}

	for ancestor := path.Dir(resolved); ancestor != c.dir && len(ancestor) > len(c.dir); ancestor = path.Dir(ancestor) {
		found, existsErr := c.pathExists(ctx, ancestor)
		if existsErr != nil {
			return "", existsErr
		}

		if found {
			return resolved, c.rejectLinkEscape(ctx, ancestor, rel)
		}
	}

	return resolved, c.rejectLinkEscape(ctx, c.dir, rel)
}

// lexicalResolve is resolveAgentPath's string half, written against POSIX paths because the tree it confines is a container's and a container's paths are always POSIX, whatever this process is running on.
func lexicalResolve(dir, rel string) (string, error) {
	resolved := path.Clean(rel)
	if !path.IsAbs(resolved) {
		resolved = path.Clean(path.Join(dir, rel))
	}

	base := path.Clean(dir)

	if resolved != base && !strings.HasPrefix(resolved, base+"/") {
		return "", fmt.Errorf("path %q escapes the working directory", rel)
	}

	return resolved, nil
}

// rejectLinkEscape asks the container where a path really leads. A path with no target is not an escape — the tool that opens it will say it is missing, and there is nothing to leak from a name pointing at nothing — which is the same answer the host gives for an unresolvable link.
func (c containerTree) rejectLinkEscape(ctx context.Context, resolved, rel string) error {
	target, err := c.realPath(ctx, resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	realBase, err := c.realPath(ctx, c.dir)
	if err != nil {
		return err
	}

	if target != realBase && !strings.HasPrefix(target, realBase+"/") {
		return fmt.Errorf("path %q escapes the working directory (resolves to %q via a symlink)", rel, target)
	}

	return nil
}

// realPath is readlink -f, present in both busybox and GNU coreutils.
func (c containerTree) realPath(ctx context.Context, p string) (string, error) {
	out, code, err := c.read(ctx, `readlink -f -- `+shQuote(p))
	if err != nil {
		return "", err
	}

	if code != 0 {
		return "", fmt.Errorf("readlink %s: %w", p, os.ErrNotExist)
	}

	resolved := strings.TrimSpace(out)
	if resolved == "" {
		return "", fmt.Errorf("readlink %s: %w", p, os.ErrNotExist)
	}

	return resolved, nil
}

// pathExists answers whether anything is at p, following links the way the tools that will open it do.
func (c containerTree) pathExists(ctx context.Context, p string) (bool, error) {
	out, code, err := c.read(ctx, `[ -e `+shQuote(p)+` ] && printf yes`)
	if err != nil {
		return false, err
	}

	return code == 0 && strings.TrimSpace(out) == "yes", nil
}
