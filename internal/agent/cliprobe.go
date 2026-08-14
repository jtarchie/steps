package agent

// Preflight for a CLI target: is the binary there, and can it authenticate?

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// probeCLI answers preflight's question for a CLI target: is the binary
// there?
//
// On the host path that is deliberately all it asks. The HTTP probe sends a
// real request because an endpoint can be reachable and still reject the
// model or the key; a host CLI has no equivalent failure that a cheap check
// would catch, and spawning one to find out would put a process launch in the
// path of every `steps watch` poll. A CLI that is installed but broken fails
// at the step, with the CLI's own error, which is a better message than a
// probe would synthesize.
//
// A CONTAINERIZED CLI inverts that trade, so it gets two more checks — see
// probeCLIImage and probeCLICredentials for why each earns its cost.
func probeCLI(ctx context.Context, ri config.ResolvedInvocation, timeout time.Duration) error {
	binary := config.CLIBinary(ri.CLI)
	if binary == "" {
		return fmt.Errorf("agent %q: no runtime for cli %q", ri.AgentName, ri.CLI)
	}

	if ri.Image == "" {
		_, err := exec.LookPath(binary)
		if err != nil {
			return fmt.Errorf("agent %q: cli %q is not on PATH: %w", ri.AgentName, binary, err)
		}

		return nil
	}

	err := probeCLICredentials(ri)
	if err != nil {
		return err
	}

	return probeCLIImage(ctx, ri, binary, timeout)
}

// probeCLIImage checks that the image actually contains the CLI. Unlike the
// host case, "installed but broken" is not the failure mode worth worrying
// about here — "the operator pointed image: at something that never had the
// CLI in it" is, and it is both easy to do and invisible until a step runs.
// The cost of asking is one short container start, paid once per (image, cli,
// model) per cache window rather than per poll.
//
// --pull=never is what keeps that cost bounded. RunJob pulls every image
// before it reaches preflight, so the image is already local; without the
// flag, an image that somehow is not would be pulled inside this probe's
// timeout, turning a slow download into "the image cannot run the cli" —
// blaming the image for the network. A genuinely absent image fails here
// with docker saying exactly that, which is the truth.
func probeCLIImage(ctx context.Context, ri config.ResolvedInvocation, binary string, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var errBuf bytes.Buffer

	//nolint:gosec // image is validated at load (no leading '-') and binary comes from the static cliProviders table
	cmd := exec.CommandContext(probeCtx, "docker", "run", "--rm", "--pull=never", "--", ri.Image, binary, "--version")
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("agent %q: image %q cannot run %q (%s): %w",
			ri.AgentName, ri.Image, binary, strings.TrimSpace(errBuf.String()), err)
	}

	return nil
}

// probeCLICredentials checks that a containerized CLI has some way to
// authenticate, because neither route is guaranteed to exist and the failure
// is otherwise reported from inside a container as whatever the CLI says
// about being logged out.
//
// Two routes, and which one is available is a property of the MACHINE, not of
// the pipeline — which is exactly why this is a preflight check and not a
// load-time one (a pipeline must not stop loading because it moved to a Mac).
// On Linux a subscription login leaves ~/.claude/.credentials.json, which the
// run mounts read-only. On macOS it lives in the Keychain, which cannot be
// mounted into a container at all, so there api_key_env: is the only route.
func probeCLICredentials(ri config.ResolvedInvocation) error {
	if _, ok := hostCLICredentials(); ok {
		return nil
	}

	if ri.APIKeyEnv != "" && os.Getenv(ri.APIKeyEnv) != "" {
		return nil
	}

	return fmt.Errorf("agent %q: a containerized cli agent has no way to authenticate: "+
		"there is no ~/.claude/.credentials.json to mount (on macOS the subscription login lives in the Keychain, "+
		"which a container cannot read) and source.api_key_env is %s",
		ri.AgentName, describeMissingKeyEnv(ri.APIKeyEnv))
}

// describeMissingKeyEnv distinguishes "you never named a variable" from "you
// named one that is not exported" — different mistakes with different fixes.
func describeMissingKeyEnv(name string) string {
	if name == "" {
		return "unset"
	}

	return fmt.Sprintf("%q, which is not exported", name)
}
