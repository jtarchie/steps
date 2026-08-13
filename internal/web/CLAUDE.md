# internal/web

`internal/web` (echo + `html/template` + one CSS file, all `go:embed`ed) serves the browser UI over the same state the CLI writes. The rules that keep this from becoming a second product:

- **web sits above pipeline**, like trigger: it reaches execution only through `pipeline.RunJob`, never into agent/resource/merkle. depguard enforces it.
- **Recording is the runner's job.** `RunJob` attaches a store-backed bus when the caller supplied none, so EVERY run persists its events, not just watched ones. A run started from a terminal and one started from the browser leave the same record.
- **The live view and the post-hoc view read the same rows.** SSE polls `run_events` by sequence number rather than subscribing to the in-process bus — that is what lets the UI show runs another process started, and what stops a live view and a replayed one from drifting.

`internal/events` is the leaf that carries run events here — see [../events/CLAUDE.md](../events/CLAUDE.md).
