/// <reference path="../../docs/src/global.d.ts" />
// The builder-closure twin of ./workflow.ts — the SAME two-gate codegen
// machine, written with state(name, s => ...) instead of object literals. It
// lowers to the same enforced graph and runs against the same
// mock_responses.yaml, producing the exact trace of the object form
// (TestCodegenMockTrace / TestCodegenBuilderParity).
//
// Why a builder: a state is assembled by CALLING setters, so fields can be
// added conditionally or in a loop — see `generate` below, whose distill
// sources and prompt sections are built up in plain JS. Each setter records
// plain data; the result is byte-identical to the literal, so every field is
// still dry-run at load. The optional name argument must match the states: key
// (a load-time guard against drift). structure stays data; the builder just
// assembles it.

const unfence = (s) =>
  s
    .replace(/^[ \t]*```[\w.+-]*[ \t]*\r?\n/, "")
    .replace(/\r?\n[ \t]*```[ \t]*$/, "");

// The architect: prose -> a concrete work-list plus the contract every file
// must honour. Chained setters read like a fluent spec of the micro-agent.
const plan = state("plan", (s) =>
  s
    .model("architect")
    .maxOutputTokens(16384)
    .reasoning("low")
    .prompt(({ spec, language }) => `
      You are a software architect. Turn this specification into a concrete
      implementation plan for a ${language} project. List every file to
      create, each with a one-line purpose. State the public contract (the
      behaviour and signatures callers depend on) and the acceptance criteria
      a reviewer will check. Do NOT write any code yet.
      SPEC:
      ${spec}`)
    .output({
      files: [{ path: "string", purpose: "string" }],
      contract: "string",
      acceptance: "string[]",
    }));

// The coder: one hermetic micro-context per planned file. The builder lets the
// distill map be assembled from a plain array — each entry is a scope slice the
// coder needs, declared once and appended in a loop instead of hand-writing the
// object literal. Byte-identical to workflow.ts's distill: block.
const DISTILL_SLICES = {
  spec: {
    for: ({ target }) =>
      `only what is needed to implement ${target.path} (${target.purpose})`,
    maxTokens: 400,
  },
  build_cause: {
    from: "build",
    for:
      "the root-cause error(s) only — exact messages with file and line, nothing else",
    maxTokens: 200,
  },
};

const generate = state("generate", (s) => {
  s.forEach({ over: ({ plan }) => plan.files, as: "target", concurrency: 3 });
  s.memo();
  s.model("coder");
  s.maxOutputTokens(32768);
  s.distill(DISTILL_SLICES); // built above; a loop could assemble it per-target
  s.prompt(({ spec, language, plan, target, review, build_cause }) => `
    Write the COMPLETE contents of exactly one file for a ${language}
    project. Output ONLY the raw file body — no markdown fences, no
    commentary, no JSON, nothing before or after the code.
    FILE: ${target.path}
    PURPOSE: ${target.purpose}
    PUBLIC CONTRACT (every file must honour this):
    ${plan.contract}
    SPEC (the slice relevant to this file):
    ${spec}
    ${
    review
      ? "A reviewer rejected the previous attempt:\n" + list(review.issues) +
        "\nAddress every issue."
      : ""
  }
    ${
    build_cause
      ? "The build/test command FAILED last time. Root cause:\n" + build_cause +
        "\nFix the underlying cause; do not paper over it."
      : ""
  }`);
});

// Gate one — the reader. Scores the full generated tree and proposes an event;
// the loop's accept: guard disposes on the score.
const review = state("review", (s) =>
  s
    .model("reviewer")
    .maxOutputTokens(32768)
    .reasoning("low")
    .prompt(({ spec, plan, generate }) => `
      You are a strict reviewer. Decide whether these files satisfy the spec,
      honour the contract, and meet EVERY acceptance criterion. Score 0-10 and
      list at most five concrete issues (each under 25 words). Approve only if
      you would merge this as-is.
      SPEC:
      ${spec}
      ACCEPTANCE CRITERIA:
      ${list(plan.acceptance)}
      FILES:
      ${
      generate.items.map((f, i) =>
        `--- ${plan.files[i].path} ---\n${unfence(f.text)}`
      ).join("\n\n")
    }`)
    .output({
      score: "number",
      issues: { type: "array", items: "string", maxItems: 5 },
    })
    .events("approve", "revise"));

// A human decides: write the current draft anyway, or fail. Reached on
// exhausted revisions (else) or a reviewer that blew its token budget (catch).
const escalate = state("escalate", (s) =>
  s
    .human(({ review }) => `
      The reviewer did not approve${
      review && review.score !== undefined
        ? ` (last score ${review.score})`
        : " (its own token budget was exhausted)"
    }.
      Write the current files anyway and let the build gate judge, or fail?`)
    .timeout("1h"));

// Boundary effect: materialise each generated file. A foreach over an ACTION —
// every write is its own journal entry, independently retryable/resumable.
const write_files = state("write_files", (s) =>
  s
    .forEach({
      over: ({ plan, generate }) =>
        plan.files.map((f, i) => ({
          path: f.path,
          content: unfence(generate.items[i].text),
        })),
      as: "file",
    })
    .action("file.write")
    .input(({ out, file }) => ({
      path: `${out}/${file.path}`,
      content: file.content,
    })));

// Gate two — the ground truth. Runs the operator's verify command in `out`;
// exec.run returns the exit code as DATA, so the loop guards route on output.ok.
const build = state("build", (s) =>
  s
    .action("exec.run")
    .input(({ out, verify_cmd }) => ({ cmd: verify_cmd, cwd: out }))
    .output({
      ok: "boolean",
      exit_code: "number",
      stdout: "string",
      stderr: "string",
      cmd: "string",
    }));

// The build stayed red past the retry budget — the human tie-break, now with
// the command's own stderr in hand.
const accept_build = state("accept_build", (s) =>
  s
    .human(({ build }) => `
      The build/test command "${build.cmd}" is still failing after retries:
      ${build.stderr}
      Accept the generated code as-is, or fail the run?`)
    .timeout("1h"));

// Terminal artifact: a manifest of what was generated and whether gate two
// went green. write: may be a function of scope too.
const report = state("report", (s) =>
  s
    .write(({ out }) => `${out}/GENERATED.md`)
    .content(({ plan, build }) => `
# Generated project

${list(plan.files.map((f) => f.path))}

**Build gate:** ${
      build.ok
        ? "PASSED"
        : `FAILED (exit ${build.exit_code}) — accepted by a human`
    }
_command:_ \`${build.cmd}\`

## Contract
${plan.contract}
`));

export default {
  name: "codegen",
  input: {
    spec: { type: "string", required: true },
    language: { type: "string", required: true },
    out: { type: "string", required: true },
    verify_cmd: { type: "string", required: true },
  },
  models: {
    architect: "openrouter/qwen/qwen3-235b-a22b-2507",
    coder: "openrouter/qwen/qwen3-coder-30b-a3b-instruct",
    reviewer: "openrouter/qwen/qwen3-235b-a22b-2507",
    distiller: "openrouter/qwen/qwen3-coder-30b-a3b-instruct",
  },
  model: "coder",
  limits: { maxTransitions: 40, maxTokens: 400000, timeout: "1h" },

  states: {
    plan,
    generate,
    review,
    escalate,
    write_files,
    build,
    accept_build,
    report,
  },

  // Identical topology to workflow.ts: two bounded loops, one per gate.
  flow: pipe(
    plan,
    loop(generate, {
      judge: review,
      accept: ({ output }) => output.score >= 8,
      maxVisits: 5,
      catch: { budget_exceeded: escalate },
      exhausted: branch(escalate, {
        approved: write_files,
        rejected: fail,
        timeout: fail,
      }),
      then: loop(write_files, {
        judge: build,
        accept: ({ output }) => output.ok,
        revise: generate,
        maxVisits: 4,
        then: report,
        exhausted: branch(accept_build, {
          approved: report,
          rejected: fail,
          timeout: fail,
        }),
      }),
    }),
  ),
} satisfies Machine;
