package shell

// A one-shot foreground container for a process the caller drives itself —
// argv-based (no `sh -c`), stdin attached, stdout read as it streams. This
// exists for internal/agent's containerized CLI subprocess, which the
// session-container Runner deliberately does not cover: Runner's interface is
// a command string executed to completion with captured output, and the CLI is
// a long-running child whose transcript is parsed turn by turn, so that a step
// which times out mid-conversation still has what it managed to do.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// hostGatewayMapping makes the host reachable from inside the container by a
// name that works on Docker Desktop and Linux Docker Engine alike.
// HostGatewayName is the container-side spelling callers must dial.
const hostGatewayMapping = HostGatewayName + ":host-gateway"

// HostGatewayName is how a containerized process names the host its parent is
// listening on. Exported because the parent has to hand the child a URL built
// from it, and the name and the --add-host mapping that makes it resolve must
// not drift apart.
const HostGatewayName = "host.docker.internal"

// Mount is one extra bind mount for a foreground run, beyond the working
// directory that ResolvedCwd already mounts.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// DockerRunSpec describes a one-shot foreground container run. The first
// block mirrors RunnerSpec's container knobs; ExtraMounts/ExtraEnv carry what
// a foreground process additionally needs (a $HOME, a credentials file).
type DockerRunSpec struct {
	// Image to run. Argv is the container's command — a binary and its
	// arguments, passed positionally after "--" with no shell in between, so
	// argument values never need shell-quoting.
	Image string
	Argv  []string
	// Name is the container's --name. It is what makes the run RECLAIMABLE:
	// nothing this end does stops a container, so a caller whose context is
	// canceled can only tear it down if it knows its name. Mint one with NewContainerName and remove it with
	// RemoveContainer. Empty lets docker name it, which should be reserved
	// for runs nobody needs to interrupt.
	Name string
	// ResolvedCwd is bind-mounted at its own path and set as the workdir,
	// exactly like a session container (see dockerStartArgs). Callers resolve
	// it with ResolveMountPath. Empty mounts nothing.
	ResolvedCwd string
	// EnvNames are the variable NAMES the pipeline opted in; their values are
	// resolved from this process's environment, and a name it does not have is
	// simply absent from the container.
	EnvNames    []string
	User        string
	Network     string
	Privileged  bool
	CPUShares   int
	MemoryBytes int64
	// ExtraMounts are additional bind mounts (`-v host:container[:ro]`).
	ExtraMounts []Mount
	// ExtraEnv are variables the caller supplies with their values, for the
	// ones this process's own environment does not hold.
	//
	// This used to be documented as non-secret values only, because it became
	// `-e NAME=value` in an argv the host's process list could read, and
	// EnvNames existed to avoid exactly that. Neither is true any more: both
	// travel in a request body, so the distinction between them is now only
	// where the value comes from.
	ExtraEnv map[string]string
}

// ForegroundRun is a one-shot container this process is driving.
type ForegroundRun struct {
	// Stdout is the container's transcript, readable as it arrives. Wait
	// closes it, so every read happens first.
	Stdout io.Reader

	attached *dockerapi.Attached
	client   *dockerapi.Client
}

// Close releases the connection the run holds.
//
// Wait does this on the way out, so the ordinary path needs nothing. It is
// exported for the path that has no ordinary way out: a caller whose context
// was cancelled abandons the run without waiting for it, and the connection —
// and the pool goroutines behind it — would outlive the step that opened it.
//
// Safe to repeat, which callers rely on: a deferred Close beside the one Wait
// already did is the shape they actually write. It is safe because closing a
// client only releases idle connections, not because anything here counts —
// a Close that ever grows state of its own has to make itself idempotent.
func (r *ForegroundRun) Close() {
	_ = r.client.Close()
}

// StartForeground starts a container from spec with its streams attached.
//
// Attached before started, so nothing the process says before this end is
// listening is lost — for a CLI whose first line announces the session, that
// line IS the thing a caller needs.
func StartForeground(ctx context.Context, spec DockerRunSpec, stdin io.Reader, stderr io.Writer) (*ForegroundRun, error) {
	client, err := dockerapi.New("")
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	attached, err := client.StartAttached(ctx, foregroundSpec(spec), stdin, stderr)
	if err != nil {
		_ = client.Close()

		return nil, fmt.Errorf("%w", err)
	}

	return &ForegroundRun{Stdout: attached.Stdout, attached: attached, client: client}, nil
}

// Wait ends the run and reports a nonzero exit as an error, matching what
// exec.Cmd's Wait does for the host path this sits beside — the caller has one
// branch for both.
func (r *ForegroundRun) Wait(ctx context.Context) error {
	defer r.Close()

	code, err := r.attached.Wait(ctx)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if code != 0 {
		return &ExitError{Command: "the cli", Code: code}
	}

	return nil
}

// foregroundSpec translates a DockerRunSpec into the container to create.
//
// --rm rather than explicit removal: the container's lifetime IS the
// process's, there is no postmortem to keep (the caller holds its stderr), and
// the daemon removes it even if this process is gone by then. The ownership
// labels still go on: a SIGKILLed steps can leave the child running inside a
// container nothing will remove until it exits, and the labels are what let
// SweepOrphanedContainers reclaim it from the next run.
func foregroundSpec(spec DockerRunSpec) dockerapi.ContainerSpec {
	return dockerapi.ContainerSpec{
		Image:       spec.Image,
		Cmd:         spec.Argv,
		Name:        spec.Name,
		WorkingDir:  spec.ResolvedCwd,
		Env:         foregroundEnv(spec),
		Labels:      OwnershipLabels(),
		User:        spec.User,
		Network:     spec.Network,
		Privileged:  spec.Privileged,
		CPUShares:   int64(spec.CPUShares),
		MemoryBytes: spec.MemoryBytes,
		Init:        true,
		AutoRemove:  true,
		Mounts:      foregroundMounts(spec),
		OpenStdin:   true,
		// Unconditionally, including under `network: host`, and the
		// counterintuitive part is that it is still NEEDED there: when the
		// daemon runs in a VM (Docker Desktop, colima) "host" networking means
		// the VM's namespace, not this machine's, so a server bound on this
		// loopback is unreachable from such a container. Treating host
		// networking as "loopback already works" is only true when the daemon
		// shares this kernel, which steps cannot tell from here.
		ExtraHosts: []string{hostGatewayMapping},
	}
}

// foregroundEnv resolves the names the pipeline opted in, then the values the
// caller supplied. A name this process does not have is omitted rather than
// set empty, the same rule the session container follows.
func foregroundEnv(spec DockerRunSpec) []string {
	env := make([]string, 0, len(spec.EnvNames)+len(spec.ExtraEnv))

	for _, name := range spec.EnvNames {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	// Sorted, so two runs of the same step describe the same container.
	for _, name := range sortedKeys(spec.ExtraEnv) {
		env = append(env, name+"="+spec.ExtraEnv[name])
	}

	return env
}

// foregroundMounts spells the extra bind mounts the way the daemon takes them.
func foregroundMounts(spec DockerRunSpec) []string {
	mounts := make([]string, 0, len(spec.ExtraMounts))

	for _, mount := range spec.ExtraMounts {
		volume := mount.HostPath + ":" + mount.ContainerPath
		if mount.ReadOnly {
			volume += ":ro"
		}

		mounts = append(mounts, volume)
	}

	return mounts
}

// sortedKeys is a deterministic order for a map of environment values.
func sortedKeys(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
