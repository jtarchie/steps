/// <reference path="../../docs/src/global.d.ts" />
// The higher-level-DSL twin of ./workflow.ts — same machine, same
// mock_responses.yaml, same trace (compare `steps validate --print`). Four
// sugars do the work:
//   - evidence: the ARTICLE:/SUMMARY: plumbing and the conditional revision
//     feedback are declared as data; prompt: is only the instruction.
//   - verdict: the judge declares its acceptance test once.
//   - loop escalate: the human tie-break is one option — the gate state,
//     its approve-rejoins-then routing, and timeout -> fail are synthesized.

const draft: State = {
  prompt:
    "Summarize the article below in at most 150 words, then give exactly three key points.",
  evidence: {
    article: true,
    reviewer_feedback: ({ critique }) =>
      critique &&
      `Your previous draft was rejected for these reasons:\n${
        list(critique.issues)
      }\nAddress every issue.`,
  },
  output: {
    summary: "string",
    key_points: { type: "array", items: "string", minItems: 3, maxItems: 3 },
  },
};

const critique: State = {
  model: "ollama/llama3.2:3b", // different micro-agent, different model
  prompt:
    "You are a strict editor. Score the summary 0-10 for accuracy and completeness against the article. List at most three concrete issues, each under 20 words.",
  evidence: {
    article: true,
    summary: ({ draft }) => draft.summary,
  },
  output: {
    score: "number",
    issues: { type: "array", items: "string", maxItems: 3 },
  },
  verdict: ({ output }) => output.score >= 8, // the acceptance test, once
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
      judge: critique,
      maxVisits: 3,
      escalate: {
        prompt: ({ critique }) =>
          `Revisions exhausted (last score ${critique.score}). Approve the current draft or fail the run?`,
        timeout: "1h",
      },
    }),
    publish,
  ),
};
