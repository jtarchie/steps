package config

import (
	"reflect"
	"testing"
)

func TestIsolationCarriesEveryField(t *testing.T) {
	t.Parallel()

	limits := &ContainerLimits{CPU: 512, Memory: 1 << 20}
	hosts := ExtraHosts{"h.test:10.0.0.1"}
	full := Isolation{User: "u", Network: "n", ExtraHosts: hosts, Privileged: true, Limits: limits, Sandbox: true}

	tests := []struct {
		name string
		got  Isolation
		want Isolation
	}{
		{"invocation", ResolvedInvocation{User: "u", Network: "n", ExtraHosts: hosts, Privileged: true, Limits: limits, Sandbox: true}.Isolation(), full},
		{"task", ResolvedTask{User: "u", Network: "n", ExtraHosts: hosts, Privileged: true, Limits: limits, Sandbox: true}.Isolation(), full},
		{"resource type", ResourceType{User: "u", Network: "n", ExtraHosts: hosts, Privileged: true, Limits: limits, Sandbox: true}.Isolation(), full},
		{"export has no sandbox", MCPExport{User: "u", Network: "n", Privileged: true, Limits: limits}.Isolation(), Isolation{User: "u", Network: "n", Privileged: true, Limits: limits}},
	}

	for _, tt := range tests {
		if !reflect.DeepEqual(tt.got, tt.want) {
			t.Errorf("%s: Isolation() = %+v, want %+v", tt.name, tt.got, tt.want)
		}
	}
}
