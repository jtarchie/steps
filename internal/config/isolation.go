package config

// Isolation is the container-shaping settings every resolved invocation, task and export carries, collected once so a consumer does not re-spell the field list (and cannot transpose the two adjacent strings).
type Isolation struct {
	User, Network string
	Privileged    bool
	Limits        *ContainerLimits
	Sandbox       bool
}

// Isolation collects the invocation's container-shaping settings.
func (ri ResolvedInvocation) Isolation() Isolation {
	return Isolation{User: ri.User, Network: ri.Network, Privileged: ri.Privileged, Limits: ri.Limits, Sandbox: ri.Sandbox}
}

// Isolation collects the task's container-shaping settings.
func (rt ResolvedTask) Isolation() Isolation {
	return Isolation{User: rt.User, Network: rt.Network, Privileged: rt.Privileged, Limits: rt.Limits, Sandbox: rt.Sandbox}
}

// Isolation collects the resource type's container-shaping settings.
func (rt ResourceType) Isolation() Isolation {
	return Isolation{User: rt.User, Network: rt.Network, Privileged: rt.Privileged, Limits: rt.Limits, Sandbox: rt.Sandbox}
}

// Isolation of an export never sets Sandbox: an MCP export has no host-sandbox setting.
func (e MCPExport) Isolation() Isolation {
	return Isolation{User: e.User, Network: e.Network, Privileged: e.Privileged, Limits: e.Limits}
}
