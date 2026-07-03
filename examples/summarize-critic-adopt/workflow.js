// The adopt variant of summarize-critic: the drafter CONTINUES its own
// conversation across revisions (context rung 3, adopt: "self") instead of
// being re-primed with distilled feedback (rung 1). Same machine shape,
// same mock script — A/B the two context philosophies.

const draft = {
  adopt: "self", // revisits continue this state's own prior conversation
  // long loops can trim the replayed transcript for token hygiene:
  //   adopt: { from: "self", lastTurns: 6 },
  prompt: ({ article, critique }) => critique
    ? `
      Your reviewer rejected the previous draft for these reasons:
      ${list(critique.issues)}
      Revise the summary. Address every issue. Same format as before.`
    : `
      Summarize the article below in at most 150 words, then give exactly
      three key points.
      ARTICLE:
      ${article}`,
  // NOTE: on revisit the article is NOT re-sent — it is already in the
  // adopted conversation. Only the feedback is appended.
  output: {
    summary: "string",
    key_points: { type: "array", items: "string", minItems: 3, maxItems: 3 },
  },
};

const critique = {
  model: "ollama/llama3.2:3b", // fresh judge every round — deliberately NOT adopted
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
  events: ["approve", "revise"],
};

const escalate = {
  human: ({ critique }) => `Revisions exhausted (last score ${critique.score}). Approve the current draft or fail the run?`,
  timeout: "1h",
};

const publish = {
  write: "out/summary.md",
  content: ({ draft }) => `${draft.summary}\n\n${list(draft.key_points)}\n`,
};

export default {
  name: "summarize-critic-adopt",
  input: { article: "string" },
  model: "ollama/qwen3:8b",
  defaults: { maxTurns: 2, reasoning: "low" },
  limits: { maxTransitions: 12 },

  states: { draft, critique, escalate, publish },

  flow: pipe(
    draft,
    branch(critique, {
      approve: when(({ output }) => output.score >= 8).to(publish),
      revise: when(({ visits }) => visits.draft < 3).to(draft),
      else: branch(escalate, {
        approved: publish,
        rejected: fail,
        timeout: fail,
      }),
    }),
  ),
};
