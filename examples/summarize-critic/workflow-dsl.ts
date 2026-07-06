/// <reference path="../../docs/src/global.d.ts" />
// The higher-level-DSL twin of ./workflow.ts — same machine, fewer moving
// parts. It lowers to the identical enforced graph (compare with
// `steps validate --print` on either file) and runs against the same
// mock_responses.yaml. Two sugar features do the work here:
//   - verdict: the judge declares its acceptance test once (no events: + a
//     separate accept: guard restating the same score field).
//   - gate(): the exhausted-escalation human state is synthesized from a
//     prompt + approve target — no hand-written `escalate` const.

const draft: State = {
  prompt: ({ article, critique }) => `
    Summarize the article below in at most 150 words, then give exactly
    three key points.
    ${critique ? "A reviewer rejected your previous draft for these reasons:\n" + list(critique.issues) + "\nAddress every issue." : ""}
    ARTICLE:
    ${article}`,
  output: {
    summary: "string",
    key_points: { type: "array", items: "string", minItems: 3, maxItems: 3 },
  },
};

const critique: State = {
  model: "ollama/llama3.2:3b",
  prompt: ({ article, draft }) => `
    You are a strict editor. Score the summary 0-10 for accuracy and
    completeness against the article. List at most three concrete issues,
    each under 20 words.
    ARTICLE:
    ${article}
    SUMMARY:
    ${draft.summary}`,
  output: {
    score: "number",
    issues: { type: "array", items: "string", maxItems: 3 },
  },
  verdict: ({ output }) => output.score >= 8, // the acceptance test, declared once
};

const publish: State = {
  write: "out/summary.md",
  content: ({ draft }) => `${draft.summary}\n\n${list(draft.key_points)}\n`,
};

export default {
  name: "summarize-critic",
  input: { article: "string" },
  model: "ollama/qwen3:8b",
  defaults: { reasoning: "low" },
  limits: { maxTransitions: 12 },

  states: { draft, critique, publish },

  flow: pipe(
    loop(draft, {
      judge: critique, // accept: is the judge's verdict:
      maxVisits: 3,
      exhausted: gate("escalate", {
        prompt: ({ critique }) => `Revisions exhausted (last score ${critique.score}). Approve the current draft or fail the run?`,
        approve: publish, // rejected -> fail, timeout -> fail (synthesized)
        timeout: "1h",
      }),
    }),
    publish,
  ),
};
