package config

import "testing"

func TestIsolationCarriesEveryField(t *testing.T) {
	t.Parallel()

	limits := &ContainerLimits{CPU: 512, Memory: 1 << 20}
	full := Isolation{User: "u", Network: "n", Privileged: true, Limits: limits, Sandbox: true}

	tests := []struct {
		name string
		got  Isolation
		want Isolation
	}{
		{"invocation", ResolvedInvocation{User: "u", Network: "n", Privileged: true, Limits: limits, Sandbox: true}.Isolation(), full},
		{"task", ResolvedTask{User: "u", Network: "n", Privileged: true, Limits: limits, Sandbox: true}.Isolation(), full},
		{"export has no sandbox", MCPExport{User: "u", Network: "n", Privileged: true, Limits: limits}.Isolation(), Isolation{User: "u", Network: "n", Privileged: true, Limits: limits}},
	}

	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: Isolation() = %+v, want %+v", tt.name, tt.got, tt.want)
		}
	}
}
