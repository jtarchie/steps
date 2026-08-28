package shell

// The one-shot foreground container, asked of the spec it creates.
//
// These used to assert an argument vector, because a foreground run was a
// `docker run` subprocess and the argv was the only artefact. It is a
// container created through the engine API now, so what is checked is the
// container — which is the same contract one indirection earlier, and says
// what a flag block never could: an unset name is ABSENT from the
// environment, a limit that was not configured is not a configured zero, and
// a mount is a mount rather than a substring of a joined string.

import (
	"slices"
	"strings"
	"testing"
)

// specFor is the container a foreground run would create.
func specFor(spec DockerRunSpec) map[string]string {
	created := foregroundSpec(spec)

	env := map[string]string{}

	for _, entry := range created.Env {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}

	return env
}

// TestForegroundRunsArgvNotShell is the property the whole file exists for:
// the CLI's argument values (JSON, paths, prompts) reach the binary exactly as
// given, because nothing re-parses them through a shell.
func TestForegroundRunsArgvNotShell(t *testing.T) {
	t.Parallel()

	awkward := `{"role": "system", "text": "a b  c 'quoted'"}`

	created := foregroundSpec(DockerRunSpec{
		Image: "my/claude:1",
		Argv:  []string{"claude", "--append-system-prompt", awkward},
	})

	want := []string{"claude", "--append-system-prompt", awkward}
	if !slices.Equal(created.Cmd, want) {
		t.Errorf("cmd = %v, want %v", created.Cmd, want)
	}

	if created.Image != "my/claude:1" {
		t.Errorf("image = %q, want the image", created.Image)
	}
}

// TestForegroundAlwaysAddsHostGateway pins that a containerized child can
// reach a server its parent bound on the host, on Docker Desktop and Linux
// Docker Engine alike.
//
// Unconditionally, including under `network: host`, and that is the
// counterintuitive part: when the daemon runs in a VM (Docker Desktop,
// colima) "host" networking means the VM's namespace, not this machine's, so
// a server bound on this loopback is unreachable from such a container.
func TestForegroundAlwaysAddsHostGateway(t *testing.T) {
	t.Parallel()

	for _, network := range []string{"", "host", "none"} {
		created := foregroundSpec(DockerRunSpec{Image: "alpine", Network: network})

		if !slices.Contains(created.ExtraHosts, HostGatewayName+":host-gateway") {
			t.Errorf("network %q: extra hosts = %v, want the host gateway mapping", network, created.ExtraHosts)
		}
	}
}

// TestForegroundIsSelfRemovingAndOwned pins the pair that decides what happens
// when this process dies.
//
// Self-removing because the container's lifetime IS the process's and there is
// no postmortem to keep — the caller holds its stderr. Owned because a
// SIGKILLed steps can leave the child running inside a container nothing will
// remove until it exits, and the labels are what let the next run reclaim it.
func TestForegroundIsSelfRemovingAndOwned(t *testing.T) {
	t.Parallel()

	created := foregroundSpec(DockerRunSpec{Image: "alpine"})

	if !created.AutoRemove {
		t.Error("AutoRemove = false; a foreground container outliving its process has nothing to remove it")
	}

	for key, value := range OwnershipLabels() {
		if created.Labels[key] != value {
			t.Errorf("label %s = %q, want %q", key, created.Labels[key], value)
		}
	}

	// Stdin is held open because the caller feeds the prompt through it.
	if !created.OpenStdin {
		t.Error("OpenStdin = false; the CLI is fed its prompt on stdin")
	}
}

// TestForegroundNamesTheContainer pins what makes a run reclaimable: nothing
// this end does stops a container, so a caller whose context was cancelled can
// only tear it down if it knows the name.
func TestForegroundNamesTheContainer(t *testing.T) {
	t.Parallel()

	created := foregroundSpec(DockerRunSpec{Image: "alpine", Name: "steps-abc"})
	if created.Name != "steps-abc" {
		t.Errorf("name = %q, want steps-abc", created.Name)
	}
}

// TestForegroundCarriesContainerSettings pins the knobs a pipeline can set,
// and that an unset one is OMITTED rather than sent as a zero — which the
// daemon reads as "no limit", the same meaning spelled in a way that makes a
// misconfiguration look deliberate.
func TestForegroundCarriesContainerSettings(t *testing.T) {
	t.Parallel()

	created := foregroundSpec(DockerRunSpec{
		Image:       "alpine",
		ResolvedCwd: "/work",
		User:        "1000:1000",
		Network:     "none",
		Privileged:  true,
		CPUShares:   512,
		MemoryBytes: 64 << 20,
	})

	if created.User != "1000:1000" || created.Network != "none" || !created.Privileged {
		t.Errorf("user %q, network %q, privileged %v; want each passed through",
			created.User, created.Network, created.Privileged)
	}

	if created.CPUShares != 512 || created.MemoryBytes != 64<<20 {
		t.Errorf("cpu shares %d, memory %d; want each passed through", created.CPUShares, created.MemoryBytes)
	}

	if created.WorkingDir != "/work" {
		t.Errorf("working dir = %q, want /work", created.WorkingDir)
	}
}

// TestForegroundOmitsUnsetSettings is the half that matters more, split out to
// stay under the linter's complexity budget: a knob nobody configured must be
// ABSENT rather than sent as a zero. The daemon reads zero as "no limit" —
// the same meaning — but records it as a configured limit of zero, which
// makes a misconfiguration look deliberate in an inspect.
func TestForegroundOmitsUnsetSettings(t *testing.T) {
	t.Parallel()

	bare := foregroundSpec(DockerRunSpec{Image: "alpine"})

	if bare.CPUShares != 0 || bare.MemoryBytes != 0 {
		t.Errorf("an unconfigured run carries cpu %d, memory %d; want neither", bare.CPUShares, bare.MemoryBytes)
	}

	if bare.User != "" || bare.Network != "" {
		t.Errorf("an unconfigured run carries user %q, network %q; want neither", bare.User, bare.Network)
	}
}

// TestForegroundExtraMounts pins the additional bind mounts and their
// read-only flag, which is what bounds a hostile image to READING the one
// credentials file it was deliberately given.
func TestForegroundExtraMounts(t *testing.T) {
	t.Parallel()

	created := foregroundSpec(DockerRunSpec{
		Image:       "alpine",
		ResolvedCwd: "/work",
		ExtraMounts: []Mount{
			{HostPath: "/host/home", ContainerPath: "/root"},
			{HostPath: "/host/creds", ContainerPath: "/root/.creds", ReadOnly: true},
		},
	})

	want := []string{"/host/home:/root", "/host/creds:/root/.creds:ro"}
	if !slices.Equal(created.Mounts, want) {
		t.Errorf("mounts = %v, want %v", created.Mounts, want)
	}

	// The working directory is NOT among them: it is named on its own, and
	// bound at its own path on both sides so host-side readers of the same
	// tree stay coherent with what the container wrote.
	if created.WorkingDir != "/work" {
		t.Errorf("working dir = %q, want it carried separately from the extra mounts", created.WorkingDir)
	}
}

// TestForegroundEnvResolvesNamesAndOmitsUnsetOnes pins the rule both container
// paths share: a variable this process does not have is absent rather than
// empty, which is what lets a pipeline name an optional one.
func TestForegroundEnvResolvesNamesAndOmitsUnsetOnes(t *testing.T) {
	t.Setenv("STEPS_TEST_FOREGROUND_SET", "a value")

	env := specFor(DockerRunSpec{
		Image:    "alpine",
		EnvNames: []string{"STEPS_TEST_FOREGROUND_SET", "STEPS_TEST_FOREGROUND_UNSET"},
		ExtraEnv: map[string]string{"HOME": "/root"},
	})

	if env["STEPS_TEST_FOREGROUND_SET"] != "a value" {
		t.Errorf("named variable = %q, want its value from this process", env["STEPS_TEST_FOREGROUND_SET"])
	}

	if _, present := env["STEPS_TEST_FOREGROUND_UNSET"]; present {
		t.Error("a named-but-unset variable reached the container; it must be absent, not empty")
	}

	if env["HOME"] != "/root" {
		t.Errorf("HOME = %q, want the caller-supplied value", env["HOME"])
	}
}

// TestForegroundEnvIsDeterministic pins that two runs of the same step
// describe the same container, which is what makes an inspect of one
// comparable to an inspect of the next.
func TestForegroundEnvIsDeterministic(t *testing.T) {
	t.Parallel()

	spec := DockerRunSpec{
		Image:    "alpine",
		ExtraEnv: map[string]string{"B": "2", "A": "1", "C": "3"},
	}

	first := foregroundSpec(spec).Env
	if !slices.Equal(first, []string{"A=1", "B=2", "C=3"}) {
		t.Errorf("env = %v, want it sorted", first)
	}

	for range 20 {
		if !slices.Equal(foregroundSpec(spec).Env, first) {
			t.Fatalf("env varied between builds: %v then %v", first, foregroundSpec(spec).Env)
		}
	}
}
