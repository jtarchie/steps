# steps — contributor guide

A state-machine runtime for LLM micro-agents, in Go. A machine is a TypeScript
file (`workflow.ts`) — transpiled by esbuild and run on goja, both in-process,
no Node: plain state consts plus ONE flow expression. See `DESIGN.md` and
`README.md` for the philosophy.

## Development workflow

- **Run `task fmt` before every commit and resolve anything it reports.** It
  runs `golangci-lint run ./... --fix` then `gofmt -w .`; fix any lint error it
  cannot auto-fix before continuing.
- Run `task test` (full suite with the race detector) before committing changes
  to product code. `task` (default) runs `fmt` then `test`.
- Commit in logical units with clear messages (the repo uses
  Conventional-Commits-style prefixes: `feat(scope):`, `fix(scope):`, `chore:`).

## Where things live

- `machine/` — the workflow model and loader: structs (`machine.go`), TS load
  pipeline (`jsload.go`), flow combinators → transitions (`flow.go`), sugar that
  lowers to real states (`distill.go`), defaults (`defaults.go`), schema
  (`schema.go`, `compile.go`), and load-time checks (`validate.go`,
  `contract.go`, `dryrun.go`).
- `engine/` — execution: the run loop (`engine.go`), agent loop (`agent.go`),
  fork/join (`parallel.go`).
- `toolreg/` — registered actions/tools (`file.*`, `exec.run`, `http.get`, `gh.*`).
- `docs/src/global.d.ts` — the authoring surface (ambient TS types for machines).
- `examples/*/workflow.ts` — the canonical machines; they double as the
  acceptance suite (`engine/acceptance_test.go`).

## Conventions

- Sugar must **lower into the same enforced graph** and pass through
  `ApplyDefaults → Compile → Validate → CheckContracts → DryRun` unchanged.
  Synthesized states use a `#` in their name (e.g. `owner#key`) so they can
  never collide with or be destructured by user states.
- No logic in strings: structure is data, logic is a plain JS function of one
  flat scope. Guards are real JS functions (or synthesized real-JS `Dyn`).
- Add new state/machine keys to the whitelists in `jsload.go` with a good
  load-time error naming the valid options.
