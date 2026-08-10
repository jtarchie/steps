# Product

<!-- impeccable:product-schema 1 -->

## Platform

adaptive

steps is a Go CLI first: the terminal is where pipelines are authored, run, and debugged, and every capability is reachable there. Since `steps web`, it also ships a local browser UI over the same sqlite state — a read-and-operate view (run transcripts, the job dependency graph, live runs, trigger/approve/resume) rather than a second product. Design work therefore covers both CLI output and YAML ergonomics AND the web surface; the web UI is a second front end on one model, never a second model.

## Users

Engineers evaluating or adopting steps as an open-source pipeline runner — people who already think in Concourse-style `get`/`task`/`put` pipelines (or want to) and want an LLM agent invocation to be a first-class step type, not a bolt-on script. They author YAML pipelines, run them locally via `steps run`/`steps test`, and use `steps watch` for downstream-trigger automation.

## Product Purpose

Run Concourse-style pipelines where an `agent` step (tool-calling: `read_file`, `list_dir`, `run_shell`, custom tools, sub-agent delegation, MCP) sits alongside conventional resource/task/put steps, with every step content-addressed and cached in SQLite so unchanged steps are skipped on rerun. Success means pipelines that mix deterministic automation and LLM-directed work while staying reproducible, cheap to re-run, and testable.

## Positioning

Unlike Concourse or GitHub Actions (no agent-native step type) and unlike LangChain-style agent frameworks (no pipeline/resource/versioning model), steps treats an LLM agent invocation as a first-class, content-addressed, cacheable pipeline step — agent-driven work gets the same reproducibility, skip-on-no-change caching, and self-verifying test story (`steps test` + `assert:`) as any other step.

## Operating Context

Primarily terminal: `steps run|watch|test|mcp`, plus `steps web` for a local browser view of the same state. Pipelines are authored as YAML; resources are fetched via shell commands or MCP; tasks run on the host shell or in Docker; agent steps call LLM providers (OpenAI-compatible APIs, OpenRouter, local models) with tool-calling. State persists in SQLite (WAL mode). Typical workflows: authoring pipeline YAML, running/testing pipelines locally, running `steps watch` for downstream triggers, resource steps backed by `gh`/git, and optionally MCP servers with OAuth.

## Capabilities and Constraints

- The primary interface is CLI output (log lines, diagnostics), YAML pipeline syntax, and `--help`/usage text.
- `steps web` adds a local, single-user browser UI: loopback by default, no authentication (it shares the trust domain of the shell that started it), and no capability the CLI lacks. It never becomes a hosted multi-tenant service without an auth story that does not exist today.
- Undecided: whether a richer terminal UI (progress bars, interactive prompts, etc.) is ever in scope, versus staying plain-log output only.

## Brand Commitments

Project name is "steps," lowercase. No logo or visual identity beyond the name. Documentation-first open-source style: README.md for quick start, CLAUDE.md for architecture/contribution reference, docs/ for feature deep-dives.

## Evidence on Hand

- `README.md` — quick start and command overview.
- `CLAUDE.md` — lean onboarding: architecture and build/test constraints.
- `docs/` — feature-by-feature reference (agents, control flow, infra, workspace, templating, MCP).
- `examples/` — runnable example pipelines, several self-verifying via `assert:` + `steps test`.
- No testimonials, published-user stories, or usage benchmarks exist yet; do not fabricate any for an open-source project still early in adoption.

## Product Principles

1. Agent steps are first-class: same caching and testing guarantees as any other pipeline step, never a special-cased bolt-on.
2. Deterministic and cheaply re-runnable: unchanged steps skip via content-addressed (merkle) hashing.
3. One model, two front ends: the web UI reads and writes the same sqlite state and runs jobs through the same `pipeline.RunJob` as the CLI — never a parallel execution path or a second source of truth.
4. Self-verifying by default: pipelines should be testable (`assert:`/`steps test`) like code, not just runnable.
5. Trust boundaries are explicit and load-bearing: pipeline-authored execution and model-directed execution are deliberately held to different trust levels throughout.

## Accessibility & Inclusion

Terminal output should remain usable via screen readers and non-visual terminal clients: never convey meaning through color alone, and keep structured, readable plain-text output a first-class concern rather than an afterthought (e.g. graceful behavior without ANSI color when piped or unsupported).
