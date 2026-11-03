package agent

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestContainerSpecCarriesEveryIsolationField(t *testing.T) {
	t.Parallel()

	ri := config.ResolvedInvocation{
		Image: "alpine:3", User: "builder", Network: "none", Privileged: true, Sandbox: true,
		Limits: &config.ContainerLimits{CPU: 512, Memory: 1 << 20},
	}

	got := containerSpec(ri.Image, "/work", nil, ri.Isolation())

	type fields struct {
		image, cwd, user, network string
		privileged, sandbox       bool
		cpu                       int
		memory                    int64
	}

	have := fields{got.Image, got.Cwd, got.User, got.Network, got.Privileged, got.Sandbox, got.CPUShares, got.MemoryBytes}
	want := fields{"alpine:3", "/work", "builder", "none", true, true, 512, 1 << 20}

	if have != want {
		t.Errorf("containerSpec = %+v, want %+v", have, want)
	}
}
