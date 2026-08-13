# internal/events

The stdlib-only leaf carrying run events from the execution packages to `internal/web`.

- **events must stay a leaf.** Every execution package publishes to it; the moment it imports config or store, they all inherit that edge and the graph stops being acyclic. `.golangci.yml`'s depguard allow-list is what enforces this — widening it is the failure mode, not a fix.
