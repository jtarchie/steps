/// <reference path="../../docs/src/global.d.ts" />
// The higher-level-DSL twin of ./workflow.ts — the same two-gate codegen
// machine, exercising all four sugar features. It lowers to the same enforced
// graph (compare `steps validate --print`) and runs against the same
// mock_responses.yaml:
//   - model tiers: architect/coder/reviewer bundle their per-role knobs in
//     models:, so the states stop restating maxOutputTokens/reasoning/memo.
//   - verdict: the reader gate and the build gate declare their acceptance
//     test once (no events: + a separate accept: guard).
//   - forEach carry: the coder fan-out pairs each output with its planned
//     file, so review and write_files read generate.items[i].item/output
//     instead of zipping plan.files[i] back by index.
//   - gate(): both human tie-breaks are synthesized escalation states.

const unfence = (s) =>
  s
    .replace(/^[ \t]*```[\w.+-]*[ \t]*\r?\n/, "")
    .replace(/\r?\n[ \t]*```[ \t]*$/, "");

const plan: State = {
  model: "architect", // the architect tier carries maxOutputTokens + reasoning
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

const generate: State = {
  // carry: each output is paired with its planned file — no index zip.
  forEach: {
    over: ({ plan }) => plan.files,
    as: "target",
    concurrency: 3,
    carry: true,
  },
  model: "coder", // the coder tier carries memo + maxOutputTokens
  distill: {
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
  }`,
};

const review: State = {
  model: "reviewer", // the reviewer tier carries maxOutputTokens + reasoning
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
    ${
    generate.items.map((e) =>
      `--- ${e.item.path} ---\n${unfence(e.output.text)}`
    ).join("\n\n")
  }`,
  output: {
    score: "number",
    issues: { type: "array", items: "string", maxItems: 5 },
  },
  verdict: ({ output }) => output.score >= 8, // the reader gate's acceptance test
};

const write_files: State = {
  // Zip is gone: carry already pairs each body with its planned path.
  forEach: {
    over: ({ generate }) =>
      generate.items.map((e) => ({
        path: e.item.path,
        content: unfence(e.output.text),
      })),
    as: "file",
  },
  action: "file.write",
  input: ({ out, file }) => ({
    path: `${out}/${file.path}`,
    content: file.content,
  }),
};

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
  verdict: ({ output }) => output.ok, // the ground-truth gate: a command's exit code
};

const report: State = {
  write: ({ out }) => `${out}/GENERATED.md`,
  content: ({ plan, build }) => `
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
`,
};

// The reader tie-break, synthesized. Reached two ways — revisions exhausted
// or the reviewer choking on its own budget — so it is a const referenced by
// both the loop's exhausted: and its catch:. approve -> write_files, and the
// synthesized rejected/timeout -> fail.
const escalate = gate("escalate", {
  prompt: ({ review }) => `
    The reviewer did not approve${
    review && review.score !== undefined
      ? ` (last score ${review.score})`
      : " (its own token budget was exhausted)"
  }.
    Write the current files anyway and let the build gate judge, or fail?`,
  approve: write_files,
  timeout: "1h",
});

export default {
  name: "codegen",
  input: {
    spec: { type: "string", required: true },
    language: { type: "string", required: true },
    out: { type: "string", required: true },
    verify_cmd: { type: "string", required: true },
  },
  models: {
    architect: {
      model: "openrouter/qwen/qwen3-235b-a22b-2507",
      maxOutputTokens: 16384,
      reasoning: "low",
    },
    coder: {
      model: "openrouter/qwen/qwen3-coder-30b-a3b-instruct",
      maxOutputTokens: 32768,
      memo: true,
    },
    reviewer: {
      model: "openrouter/qwen/qwen3-235b-a22b-2507",
      maxOutputTokens: 32768,
      reasoning: "low",
    },
    distiller: "openrouter/qwen/qwen3-coder-30b-a3b-instruct",
  },
  model: "coder",
  limits: { maxTransitions: 40, maxTokens: 400000, timeout: "1h" },

  states: { plan, generate, review, write_files, build, report },

  flow: pipe(
    plan,
    loop(generate, {
      judge: review, // accept: is the reviewer's verdict:
      maxVisits: 5,
      catch: { budget_exceeded: escalate },
      exhausted: escalate,
      then: loop(write_files, {
        judge: build, // accept: is the build's verdict: (output.ok)
        revise: generate,
        maxVisits: 4,
        then: report,
        exhausted: gate("accept_build", {
          prompt: ({ build }) => `
    The build/test command "${build.cmd}" is still failing after retries:
    ${build.stderr}
    Accept the generated code as-is, or fail the run?`,
          approve: report,
          timeout: "1h",
        }),
      }),
    }),
  ),
};
