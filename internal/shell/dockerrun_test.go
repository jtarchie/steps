package shell

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// argAfter returns the value following flag in args, or "" if absent.
func argAfter(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}

// positionals returns everything after the "--" separator: the image and the
// container's own argv.
func positionals(args []string) []string {
	idx := slices.Index(args, "--")
	if idx < 0 {
		return nil
	}

	return args[idx+1:]
}

// TestDockerRunArgvRunsArgvNotShell is the property the whole file exists for:
// the CLI's argument values (JSON, paths, prompts) reach the binary exactly as
// given, because nothing re-parses them through a shell.
func TestDockerRunArgvRunsArgvNotShell(t *testing.T) {
	t.Parallel()

	args := DockerRunArgv(DockerRunSpec{
		Image: "alpine",
		Argv:  []string{"claude", "--append-system-prompt", `{"a": "b c"}`},
	})

	if slices.Contains(args, "sh") || slices.Contains(args, "-c") {
		t.Errorf("args = %v, want no shell wrapper", args)
	}

	want := []string{"alpine", "claude", "--append-system-prompt", `{"a": "b c"}`}
	if got := positionals(args); !reflect.DeepEqual(got, want) {
		t.Errorf("positionals = %v, want %v", got, want)
	}
}

// TestDockerRunArgvAlwaysAddsHostGateway pins the flag that makes the parent
// process reachable from inside the container on Linux Docker Engine, where
// host.docker.internal is otherwise unresolvable. Losing it breaks every
// bridged tool call, and would do so only on Linux.
func TestDockerRunArgvAlwaysAddsHostGateway(t *testing.T) {
	t.Parallel()

	for _, spec := range []DockerRunSpec{
		{Image: "alpine", Argv: []string{"claude"}},
		{Image: "alpine", Argv: []string{"claude"}, Network: "bridge"},
		// Including host networking: when the daemon is in a VM, that "host"
		// is the VM, so this machine's loopback is still not reachable.
		{Image: "alpine", Argv: []string{"claude"}, Network: "host"},
	} {
		args := DockerRunArgv(spec)
		if got := argAfter(args, "--add-host"); got != "host.docker.internal:host-gateway" {
			t.Errorf("--add-host = %q, want host.docker.internal:host-gateway (args %v)", got, args)
		}
	}
}

// TestDockerRunArgvNamesTheContainer covers what makes a run reclaimable:
// killing the docker client does not stop the container, so a caller whose
// context is canceled can only tear it down by name.
func TestDockerRunArgvNamesTheContainer(t *testing.T) {
	t.Parallel()

	args := DockerRunArgv(DockerRunSpec{Image: "alpine", Argv: []string{"claude"}, Name: "steps-abc"})

	if got := argAfter(args, "--name"); got != "steps-abc" {
		t.Errorf("--name = %q, want steps-abc", got)
	}
}

// TestDockerRunArgvIsForegroundAndSelfRemoving distinguishes this from the
// session container: the run's lifetime is the process's lifetime.
func TestDockerRunArgvIsForegroundAndSelfRemoving(t *testing.T) {
	t.Parallel()

	args := DockerRunArgv(DockerRunSpec{Image: "alpine", Argv: []string{"claude"}})

	for _, flag := range []string{"--rm", "-i", "--init"} {
		if !slices.Contains(args, flag) {
			t.Errorf("args = %v, want %s", args, flag)
		}
	}

	if slices.Contains(args, "-d") {
		t.Errorf("args = %v, did not want -d (this run is foreground)", args)
	}

	if slices.Contains(args, "-t") {
		t.Errorf("args = %v, did not want -t", args)
	}
}

// TestDockerRunArgvCarriesContainerSettings checks the block shared with the
// session container is actually applied, since it is now reached through a
// different caller.
func TestDockerRunArgvCarriesContainerSettings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	args := DockerRunArgv(DockerRunSpec{
		Image:       "alpine",
		Argv:        []string{"claude"},
		ResolvedCwd: dir,
		EnvNames:    []string{"CUSTOM"},
		User:        "1000:1000",
		Network:     "my-compose-net",
		Privileged:  true,
		CPUShares:   512,
		MemoryBytes: 1 << 30,
	})

	checks := map[string]string{
		"-v":           dir + ":" + dir,
		"-w":           dir,
		"--user":       "1000:1000",
		"--network":    "my-compose-net",
		"--cpu-shares": "512",
		"--memory":     "1073741824",
		"--add-host":   "host.docker.internal:host-gateway",
		"--label":      dockerOwnerLabel + "=steps",
	}
	for flag, want := range checks {
		if got := argAfter(args, flag); got != want {
			t.Errorf("%s = %q, want %q (args %v)", flag, got, want, args)
		}
	}

	if !slices.Contains(args, "--privileged") {
		t.Errorf("args = %v, want --privileged", args)
	}

	// Value-free forwarding: the name appears, the value never does.
	if !slices.Contains(args, "CUSTOM") {
		t.Errorf("args = %v, want -e CUSTOM", args)
	}
}

// TestDockerRunArgvExtraMounts covers the $HOME and credentials mounts, and
// specifically that :ro is spelled only where asked — a credentials file
// mounted read-write would let a container rewrite the operator's token.
func TestDockerRunArgvExtraMounts(t *testing.T) {
	t.Parallel()

	args := DockerRunArgv(DockerRunSpec{
		Image: "alpine",
		Argv:  []string{"claude"},
		ExtraMounts: []Mount{
			{HostPath: "/tmp/home", ContainerPath: "/steps-home"},
			{HostPath: "/tmp/creds.json", ContainerPath: "/steps-home/.claude/.credentials.json", ReadOnly: true},
		},
	})

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-v /tmp/home:/steps-home",
		"-v /tmp/creds.json:/steps-home/.claude/.credentials.json:ro",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args = %v, want to contain %q", args, want)
		}
	}

	if strings.Contains(joined, "-v /tmp/home:/steps-home:ro") {
		t.Errorf("args = %v, home mount must stay writable", args)
	}
}

// TestDockerRunArgvExtraEnvIsLiteralAndSorted: ExtraEnv is for non-secret
// values (a path), so it is spelled NAME=value — unlike EnvNames, whose whole
// point is keeping values out of argv. Sorted so the argv is reproducible.
func TestDockerRunArgvExtraEnvIsLiteralAndSorted(t *testing.T) {
	t.Parallel()

	args := DockerRunArgv(DockerRunSpec{
		Image:    "alpine",
		Argv:     []string{"claude"},
		ExtraEnv: map[string]string{"HOME": "/steps-home", "AAA": "1"},
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-e AAA=1 -e HOME=/steps-home") {
		t.Errorf("args = %v, want AAA before HOME, both as NAME=value", args)
	}
}

// TestDockerRunArgvSeparatorPrecedesImage guards the same smuggling defense
// dockerStartArgs documents: an image value docker would read as a flag must
// only ever be looked up as an (invalid) image name.
func TestDockerRunArgvSeparatorPrecedesImage(t *testing.T) {
	t.Parallel()

	args := DockerRunArgv(DockerRunSpec{Image: "--privileged", Argv: []string{"claude"}})

	if got := positionals(args); len(got) == 0 || got[0] != "--privileged" {
		t.Errorf("positionals = %v, want the image first, after --", got)
	}
}
