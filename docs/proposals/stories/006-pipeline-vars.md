# 006 — Pipeline vars: `((var))` interpolation and `load_var`

**Source:** [robustness.md #7](../robustness.md#7-pipeline-vars--var-interpolation-and-load_var)
**Tier:** 1 (Concourse gap) · **Priority rationale:** standalone
infrastructure feature, independent of the fan-out/fan-in work above, but
lower urgency than the concurrency features since today's workaround
(hard-coding values, or `api_key_env` for the one secret case) is merely
inconvenient rather than unsafe or blocking.

## Feature

Separate pipeline *shape* from pipeline *parameters*: `((var))` for
load-time substitution, `load_var` for capturing a runtime value mid-build.

```yaml
# pipeline.yml
resources:
  - name: repo
    type: git-like
    source:
      uri: ((repo_uri))

# steps run pipeline.yml --job build --var repo_uri=https://... \
#   or --vars-file prod.yml
```

```yaml
- task: pick-tag
  run: git describe --tags > version.txt
- load_var: tag
  file: version.txt
- put: release
  params:
    tag: "{{ .vars.tag }}"
```

## Additional feature details

- **Secret-shaped vars need the same treatment as `api_key_env`.** The repo's
  own trust-boundary rules (see CLAUDE.md) draw a hard line: anything
  secret-adjacent is either folded into hashed content as an *env var name*
  (never a value) or kept in a token file outside `.steps/` entirely. A
  `((var))` sourced from `--vars-file` with a literal secret value would, per
  the doc's own "different vars = different cache key" design, land in
  cleartext in the resolved config's hash and potentially in `state.db` —
  that directly contradicts the existing precedent (`validateAgentEndpoints`
  rejecting userinfo in `endpoint:` for exactly this reason). This needs a
  sensitivity marker (e.g. `((secret_var))` or a `--secret-vars-file` that's
  excluded from hashing) before this ships, not as a follow-up.
- **Missing-var failure mode.** Should fail at `LoadConfig` time with a
  single message listing *every* unresolved `((var))` reference across the
  whole file — not fail one step at a time during execution, which would
  mean a long-running pipeline discovers a typo'd var name only after
  earlier steps already ran.
- **Precedence.** Need a documented resolution order when a var is set in
  multiple places: `--var` (CLI, highest), `--vars-file`, and whether a
  default-value syntax (`((var:default))`) is supported at all — recommend
  *not* supporting defaults in v1, since it adds a second way to hide what a
  pipeline actually needs to be handed.
- **`load_var` and merkle hashing.** A loaded value flows into downstream
  steps' template context the same way a pinned resource version does, so it
  should fold into those downstream nodes' hashed content — otherwise two
  runs with different captured tags would incorrectly share a cache entry.
