/// <reference path="../../docs/src/global.d.ts" />
// Spec -> working code, with TWO deterministic gates around a stochastic
// interior. An architect plans the files once; a cheap coder fans out over
// the plan (one hermetic context per file); a reviewer gates on the code
// (rung-1 ctx, LLM judgement); then a REAL build/test command gates on the
// ground truth. Either gate can send the coder back with feedback — the
// review loop fixes what a human reader would catch, the build loop fixes
// what only a compiler knows. visits bound both; a human breaks the tie.
//
// The whole point of the second gate: an LLM reviewer can be fooled, `sh -n`
// / `go build` / `pytest` cannot. Determinism at the boundary, choice in the
// interior — the interior writes code, the boundary is a command's exit code.

// Defensive: small local models sometimes fence code in ```blocks despite
// being told not to. Strip a leading/trailing fence so a stray one cannot
// poison the written file (and fail the build gate).
const unfence = (s) => s
  .replace(/^[ \t]*```[\w.+-]*[ \t]*\r?\n/, "")
  .replace(/\r?\n[ \t]*```[ \t]*$/, "");

// The architect: one strong pass turns prose into a concrete work-list plus
// the contract every generated file must honour. No code yet — planning and
// writing are different jobs, so they are different micro-agents.
const plan: State = {
  model: "architect",
  maxOutputTokens: 16384,
  reasoning: "low", // a plan needs a little thought, not a dissertation
  prompt: ({ spec, language }) => `
    You are a software architect. Turn this specification into a concrete
    implementation plan for a ${language} project. List every file to
    create, each with a one-line purpose. State the public contract (the
    behaviour and signatures callers depend on) and the acceptance criteria
    a reviewer will check. Do NOT write any code yet.
    SPEC:
    ${spec}`,
  output: {
    files: [{ path: "string", purpose: "string" }],
    contract: "string",
    acceptance: "string[]",
  },
};

// The coder: one hermetic micro-context per planned file — N small windows,
// not one big one. Every file sees the shared contract but never its
// siblings' bodies. memo: an unchanged (file, feedback) pair is free on the
// next loop, so a build failure re-pays only for the files it actually
// touches. On a revisit the two feedback channels light up: `review.issues`
// (what a reader rejected) and `build_cause` (what the command rejected).
// RAW TEXT out, deliberately NOT a JSON object. A source file is multi-line;
// packing it into a JSON `content` string is exactly where small local models
// break — they write real newlines and the JSON is invalid. Default {text}
// output keeps the file body out of any JSON envelope. The path isn't asked
// for either — it's the planned path, zipped back by index in write_files.
const generate: State = {
  forEach: { over: ({ plan }) => plan.files, as: "target", concurrency: 3 },
  memo: true,
  model: "coder",
  maxOutputTokens: 32768, // a whole source file is the biggest payload here — give it room

  // Rung 1.5 — declared context slicing (docs/distill.md). Each entry lowers
  // to a real micro-state (generate#spec, generate#build_cause) on the cheap
  // distiller model, memoized by (source, need). Inside this state, `spec`
  // IS the per-file slice — the whole document never enters a coder context,
  // and an unchanged slice keeps the coder's own memo key stable. Before the
  // first build, `build_cause` has no source yet and reads as "" for free.
  distill: {
    spec: {
      for: ({ target }) => `only what is needed to implement ${target.path} (${target.purpose})`,
      maxTokens: 400,
    },
    build_cause: {
      from: "build",
      for: "the root-cause error(s) only — exact messages with file and line, nothing else",
      maxTokens: 200,
    },
  },

  prompt: ({ spec, language, plan, target, review, build_cause }) => `
    Write the COMPLETE contents of exactly one file for a ${language}
    project. Output ONLY the raw file body — no markdown fences, no
    commentary, no JSON, nothing before or after the code.
    FILE: ${target.path}
    PURPOSE: ${target.purpose}
    PUBLIC CONTRACT (every file must honour this):
    ${plan.contract}
    SPEC (the slice relevant to this file):
    ${spec}
    ${review ? "A reviewer rejected the previous attempt:\n" + list(review.issues) + "\nAddress every issue." : ""}
    ${build_cause ? "The build/test command FAILED last time. Root cause:\n" + build_cause + "\nFix the underlying cause; do not paper over it." : ""}`,
  // no output schema -> default { text: string }: generate.items[i].text is
  // the raw body of plan.files[i].
};

// Gate one — the reader. Sees the full generated tree (rung 1: outputs
// templated in) and the acceptance criteria; scores and, on approve, lets it
// reach disk. Agent proposes the event, the guard disposes on the score.
const review: State = {
  model: "reviewer",
  maxOutputTokens: 32768,
  reasoning: "low", // judge against the criteria; don't re-derive the project
  prompt: ({ spec, plan, generate }) => `
    You are a strict reviewer. Decide whether these files satisfy the spec,
    honour the contract, and meet EVERY acceptance criterion. Score 0-10 and
    list at most five concrete issues (each under 25 words). Approve only if
    you would merge this as-is.
    SPEC:
    ${spec}
    ACCEPTANCE CRITERIA:
    ${list(plan.acceptance)}
    FILES:
    ${generate.items.map((f, i) => `--- ${plan.files[i].path} ---\n${unfence(f.text)}`).join("\n\n")}`,
  output: {
    score: "number",
    issues: { type: "array", items: "string", maxItems: 5 }, // schema = output budget
  },
  events: ["approve", "revise"],
};

// A human decides: write the current draft anyway (and let the build gate
// judge), or fail the run. Reached two ways — revisions exhausted (`else`),
// or the reviewer model itself choked on its token budget (`catch`) — so the
// score may be absent.
const escalate: State = {
  human: ({ review }) => `
    The reviewer did not approve${review && review.score !== undefined ? ` (last score ${review.score})` : " (its own token budget was exhausted)"}.
    Write the current files anyway and let the build gate judge, or fail?`,
  timeout: "1h",
};

// Boundary effect: materialise each generated file. A foreach over an ACTION
// — every write is its own journal entry, independently retryable/resumable.
// `out` is the checkout the build command runs in.
const write_files: State = {
  // Zip each generated body back to its planned path by index, stripping any
  // stray fence on the way to disk.
  forEach: {
    over: ({ plan, generate }) => plan.files.map((f, i) => ({ path: f.path, content: unfence(generate.items[i].text) })),
    as: "file",
  },
  action: "file.write",
  input: ({ out, file }) => ({ path: `${out}/${file.path}`, content: file.content }),
};

// Gate two — the ground truth. Runs the operator-supplied verify command in
// `out`. exec.run returns the exit code as DATA (never an exception on a
// failed build), so the guards below route on `output.ok` instead of the
// engine retrying the same broken tree. The `cmd` is a run input, never
// model text — the model chooses the code, the operator chooses the check.
const build: State = {
  action: "exec.run",
  input: ({ out, verify_cmd }) => ({ cmd: verify_cmd, cwd: out }),
  output: {
    ok: "boolean",
    exit_code: "number",
    stdout: "string",
    stderr: "string",
    cmd: "string",
  },
};

// The build stayed red past the retry budget — the same human tie-break, now
// with the command's own stderr in hand.
const accept_build: State = {
  human: ({ build }) => `
    The build/test command "${build.cmd}" is still failing after retries:
    ${build.stderr}
    Accept the generated code as-is, or fail the run?`,
  timeout: "1h",
};

// Terminal artifact: a manifest recording what was generated and whether the
// ground-truth gate went green. `write:` may be a function of scope too.
const report: State = {
  write: ({ out }) => `${out}/GENERATED.md`,
  content: ({ plan, build }) => `
# Generated project

${list(plan.files.map(f => f.path))}

**Build gate:** ${build.ok ? "PASSED" : `FAILED (exit ${build.exit_code}) — accepted by a human`}
_command:_ \`${build.cmd}\`

## Contract
${plan.contract}
`,
};

export default {
  name: "codegen",
  input: {
    spec: { type: "string", required: true },       // the feature, in prose
    language: { type: "string", required: true },    // names the target; keeps the machine generic
    out: { type: "string", required: true },         // checkout dir the files land in and the build runs in
    verify_cmd: { type: "string", required: true },  // the ground-truth gate, e.g. "go build ./..." or "pytest -q"
  },
  models: {
    // The gates run on OpenRouter (OPENROUTER_API_KEY), the bulk coder stays
    // local and free. Unlike LM Studio, OpenRouter honors reasoning_effort, so
    // the `reasoning:` knob on the gate states is live and bounds the thinking
    // — which is why these gates behave here where they ran away locally.
    architect: "openrouter/qwen/qwen3.6-27b",       // larger model: one careful plan
    coder: "openrouter/qwen/qwen3-coder-flash",      // a real coder model, fanned out per file
    reviewer: "openrouter/qwen/qwen3.6-27b",         // the reader gate; spent sparingly
    distiller: "openrouter/qwen/qwen3-coder-flash",  // extraction is a small-model job; lmstudio/… works too
  },
  model: "coder",
  limits: { maxTransitions: 40, maxTokens: 400000, timeout: "1h" }, // generous wall clock: a local coder is slow and gates may park for a human

  states: { plan, generate, review, escalate, write_files, build, accept_build, report },

  // Two bounded loops, one per gate — and each loop owns its own budget,
  // bound on the gate that observes it (visits.review / visits.build).
  // Measured live before that split, the reader loop spent the shared budget
  // and the build loop got one shot.
  flow: pipe(
    plan,
    // Gate one — the reader. Reject revises the coder while the budget
    // lasts; exhausted escalates to a human; accept proceeds to gate two.
    loop(generate, {
      judge: review,
      accept: ({ output }) => output.score >= 8,
      maxVisits: 5, // reader loop — a few passes to converge
      // Defensive: if a reviewer model ever burns its whole budget thinking
      // (common on local backends that ignore reasoning_effort), don't kill
      // the run — route it to the same human tie-break.
      catch: { budget_exceeded: escalate },
      exhausted: branch(escalate, { approved: write_files, rejected: fail, timeout: fail }),
      // Gate two — the ground truth. Materialise, then let the command's
      // exit code judge (an action state judges as well as a model: accept
      // reads output.ok, not a score). Red loops the coder with the
      // distilled root cause — revise re-enters UPSTREAM of this loop's
      // body, which is why it is explicit.
      then: loop(write_files, {
        judge: build,
        accept: ({ output }) => output.ok,
        revise: generate, // build loop: 3 red retries, reader-independent
        maxVisits: 4,
        then: report,
        exhausted: branch(accept_build, { approved: report, rejected: fail, timeout: fail }),
      }),
    }),
  ),
};
