# 005 — `across:` matrix fan-out

**Source:** [robustness.md #5](../robustness.md#5-across--matrix-fan-out)
**Tier:** 1 (Concourse gap) · **Priority rationale:** an aggregation policy
built directly on [002](002-in-parallel-fan-out.md)'s branch runner — the
last of the pure Concourse-parity fan-out features before the agent-native
tier (ensemble/race) that reuse the same machinery for different purposes.

## Feature

Run the same step (or steps) once per combination of declared values —
useful for "test against every Go version" or "run this agent against every
package."

```yaml
- across:
    - var: go_version
      values: ["1.25", "1.26"]
    - var: package
      values: [internal/agent, internal/pipeline]
  task: matrix-test
  image: "golang:{{ .vars.go_version }}"
  run: go test ./{{ .vars.package }}/...
```

## Additional feature details

- **Combinatorial explosion guard.** Two or three `across:` axes multiply
  fast (2×2 is fine, 5×5×5 is 125 steps). The feature should warn — or cap —
  when the resulting cell count crosses a sane threshold, so a typo'd values
  list doesn't silently launch hundreds of tasks/containers/model calls.
- **Shared config with `in_parallel`.** `limit:`/`fail_fast:` should mean the
  same thing here as in `in_parallel`, both for implementation reuse and so
  users don't have to learn two vocabularies for the same concept.
- **Per-cell caching is the headline win over Concourse.** Concourse re-runs
  the entire matrix on any change; because `steps` hashes each cell's vars
  into its own merkle node, changing one value in one axis re-runs only the
  affected cells. This should be called out prominently in docs/examples —
  it's a genuine differentiator, not just parity.
- **Downstream consumption of per-cell outputs.** If a matrix task declares
  `outputs:`, how does a later step reference one specific cell's output
  versus all of them? This is genuinely unresolved in the source doc —
  recommend treating it as an open question for the design phase rather than
  deciding now: the safe default is "matrix steps aren't individually
  addressable downstream, only the aggregate pass/fail is," with per-cell
  addressing as a possible follow-up if a real use case needs it.
- **Var naming collisions.** `var:` names must not collide with existing
  template namespaces (`.source`, `.params`, `.args`) — worth an explicit
  load-time check given `.vars` is a new namespace being introduced
  alongside [006](006-pipeline-vars.md)'s `((var))`/`load_var` (same name,
  different mechanism — needs a clear docs distinction between the two).
