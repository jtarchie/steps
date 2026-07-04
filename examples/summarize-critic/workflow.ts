/// <reference path="../../docs/src/global.d.ts" />
// Draft a summary with a small local model, have a second micro-agent
// critique it, loop with feedback until it passes or a human decides.
// Context rung 1: each revision is a FRESH conversation re-primed with the
// article + distilled feedback. Compare ../summarize-critic-adopt/.

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
  model: "ollama/llama3.2:3b", // different micro-agent, different model
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
    issues: { type: "array", items: "string", maxItems: 3 }, // schema doubles as an output budget
  },
  events: ["approve", "revise"],
};

const escalate: State = {
  human: ({ critique }) => `Revisions exhausted (last score ${critique.score}). Approve the current draft or fail the run?`,
  timeout: "1h",
};

const publish: State = {
  write: "out/summary.md",
  content: ({ draft }) => `${draft.summary}\n\n${list(draft.key_points)}\n`,
};

export default {
  name: "summarize-critic",
  input: { article: "string" },
  model: "ollama/qwen3:8b", // any OpenAI-compatible endpoint works
  defaults: {
    reasoning: "low", // reasoning tokens are billed output; summarizing needs none
  },
  limits: { maxTransitions: 12 }, // hard backstop for the revision loop

  states: { draft, critique, escalate, publish },

  flow: pipe(
    loop(draft, {
      judge: critique,
      accept: ({ output }) => output.score >= 8,
      maxVisits: 3, // the ENGINE bounds the loop: visits.critique < 3
      exhausted: branch(escalate, {
        approved: publish,
        rejected: fail,
        timeout: fail,
      }),
    }),
    publish, // accept falls through here (loop's then defaults to the pipe successor)
  ),
};
