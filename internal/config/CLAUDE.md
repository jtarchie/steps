# internal/config

YAML parsing (`LoadConfig`) and every config type (`Config`, `Resource`, `Agent`, `Task`, `Job`, `Step`, ...); also the config-merge logic (`ResolveTask`, `ResolveAgentInvocation`) both plan-time hashing and run-time execution share, plus `run_file:`/`system_file:`/`message_files:`/`file:` include resolution. Depends on nothing internal.

**One file per domain:** `config.go` is only `LoadConfig` plus the `validate()` dispatcher; every check that dispatcher names lives in the file for its feature, next to the types it validates. Add a new rule to the file that owns its feature, not to `config.go`.

Three cross-cutting pieces live here too: `strictyaml.go` decodes with `KnownFields(true)` and supplies `rejectUnknownKeys` for the hand-written `UnmarshalYAML` decoders (KnownFields does NOT reach through those — a new scalar-or-mapping decoder must call it or its keys go unchecked); `suggest.go` is the shared "did you mean" used by unknown-key and `Find*` errors; `lines.go` stamps `Job.Line`/`Step.Line` from a second node-tree parse so validation errors can cite a line. `validate()` runs every check and `errors.Join`s them — a new validator is one more entry in that slice, and should stop at its own first error.
