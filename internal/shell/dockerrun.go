package shell

// A one-shot foreground `docker run` for a process the caller drives itself —
// argv-based (no `sh -c`), stdin attached, stdout/stderr wired by the caller's
// own exec.Cmd. This exists for internal/agent's containerized CLI subprocess,
// which the session-container Runner deliberately does not cover: Runner's
// interface is a command string executed to completion with captured output,
// and the CLI is a long-running child whose stdout is parsed as it streams.

import "sort"

// Mount is one extra bind mount for DockerRunArgv, beyond the working
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
	// ResolvedCwd is bind-mounted at its own path and set as the workdir,
	// exactly like a session container (see dockerStartArgs). Callers resolve
	// it with ResolveMountPath. Empty mounts nothing.
	ResolvedCwd string
	// EnvNames are forwarded value-free (`-e NAME`), same reasoning as the
	// session container: the value must not appear in the docker client's argv.
	EnvNames    []string
	User        string
	Network     string
	Privileged  bool
	CPUShares   int
	MemoryBytes int64
	// ExtraMounts are additional bind mounts (`-v host:container[:ro]`).
	ExtraMounts []Mount
	// ExtraEnv is set as literal `-e NAME=value` — for non-secret values only
	// (a path like HOME), since argv is visible to the host's process list.
	ExtraEnv map[string]string
}

// DockerRunArgv builds the argv (after "docker") for one foreground run:
// `run --rm -i --init --add-host ... [shared flags] [extra mounts/env] --
// image argv...`.
//
// --rm rather than the session container's explicit removal: the container's
// lifetime IS the process's lifetime, there is no postmortem to keep (the
// caller holds the process's own stderr), and the daemon removes it even if
// this process is gone by then. The ownership labels still go on: a SIGKILLed
// steps can leave the child hanging inside a container nothing will remove
// until it exits, and the labels are what let SweepOrphanedContainers reclaim
// it from the next run.
//
// --add-host host.docker.internal:host-gateway makes the host reachable by
// that name on Linux Docker Engine as well as Docker Desktop (which resolves
// it natively) — it is how a containerized child reaches a server the parent
// bound on the host, regardless of platform, without claiming --network host.
func DockerRunArgv(spec DockerRunSpec) []string {
	args := []string{"run", "--rm", "-i", "--init", "--add-host", "host.docker.internal:host-gateway"}

	args = append(args, containerArgs(spec.ResolvedCwd, spec.EnvNames, spec.User, spec.Network,
		spec.Privileged, spec.CPUShares, spec.MemoryBytes)...)

	for _, mount := range spec.ExtraMounts {
		volume := mount.HostPath + ":" + mount.ContainerPath
		if mount.ReadOnly {
			volume += ":ro"
		}

		args = append(args, "-v", volume)
	}

	// Sorted so the argv is deterministic — for tests, and so two runs of the
	// same step produce the same `docker inspect` output.
	names := make([]string, 0, len(spec.ExtraEnv))
	for name := range spec.ExtraEnv {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		args = append(args, "-e", name+"="+spec.ExtraEnv[name])
	}

	args = append(args, "--", spec.Image)

	return append(args, spec.Argv...)
}
