package agent

// Asking an image, once, what it can actually do.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jtarchie/steps/internal/shell"
)

// userland is what one exec learned about the image the step's file tools will run in.
type userland struct {
	// pwd is the container's own working directory, which for a placed agent is a path on the WORKER and is therefore what the model has to be told, since every path it is handed back comes from there.
	pwd string
	// missing names the commands the file tools need and this image does not have, in the order the model would miss them.
	missing []string
}

// treeCommands are what the scripts in treecontainer.go actually invoke. Kept beside the probe so a script reaching for something new fails the step with a sentence instead of failing a tool call with whatever the shell said.
var treeCommands = []string{"find", "grep", "sed", "cat", "head", "wc", "mkdir", "readlink", "tr", "chmod", "rm"}

// errUnusableImage is an image that cannot host the step's file tools. Raised at preparation rather than at the first tool call, so a pipeline whose image is too thin costs a second instead of a conversation.
var errUnusableImage = errors.New("image cannot run this agent's file tools")

// probeUserland runs the single script answering both questions the container path needs: where its tree is, and whether the tools that read it are present. `printf` is deliberately absent from the list because it is a shell builtin in both busybox ash and bash, so `command -v` finding no file on PATH would condemn an image whose printf works perfectly.
func probeUserland(ctx context.Context, runner shell.Runner, image string) (userland, error) {
	script := `printf 'pwd=%s\n' "$(pwd)"; for b in ` + strings.Join(treeCommands, " ") +
		`; do command -v "$b" >/dev/null 2>&1 || printf 'missing=%s\n' "$b"; done`

	out, _, code, err := runner.RunCaptureFullLimited(ctx, script, probeOutputBytes, "")
	if err != nil {
		return userland{}, fmt.Errorf("%w %q: %w", errUnusableImage, image, err)
	}

	if code != 0 {
		return userland{}, fmt.Errorf("%w %q: the image has no POSIX shell to run them with", errUnusableImage, image)
	}

	found := parseUserland(out)
	if len(found.missing) > 0 {
		return userland{}, fmt.Errorf("%w %q: it has no %s — an agent with image: runs read_file, search_files and the rest inside the container, so the image has to carry the ordinary file utilities",
			errUnusableImage, image, strings.Join(found.missing, ", "))
	}

	if found.pwd == "" {
		return userland{}, fmt.Errorf("%w %q: it did not report a working directory", errUnusableImage, image)
	}

	return found, nil
}

// probeOutputBytes is a generous bound on a handful of short lines.
const probeOutputBytes = 8 << 10

func parseUserland(out string) userland {
	var found userland

	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}

		switch key {
		case "pwd":
			found.pwd = value
		case "missing":
			found.missing = append(found.missing, value)
		}
	}

	sort.Strings(found.missing)

	return found
}
