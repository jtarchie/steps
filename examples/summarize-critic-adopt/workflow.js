// The adopt variant of summarize-critic: the drafter CONTINUES its own
// conversation across revisions (context rung 3, adopt: "self") instead of
// being re-primed with distilled feedback (rung 1). Same machine shape,
// same mock script — A/B the two context philosophies.

module.exports = {
  version: 1,
  name: "summarize-critic-adopt",

  input: { article: { type: "string", required: true } },

  defaults: {
    agent: {
      model: "ollama/qwen3:8b", // any OpenAI-compatible endpoint works
      maxTurns: 2,
      reasoning: "low",
    },
  },

  limits: { maxTransitions: 12 },

  states: {
    draft: {
      agent: {
        adopt: "self", // revisits continue this state's own prior conversation
        // long loops can trim the replayed transcript for token hygiene:
        //   adopt: { from: "self", lastTurns: 6 },
        prompt: ({ ctx }) => ctx.critique
          ? `Your reviewer rejected the previous draft for these reasons:
${ctx.critique.issues.map(i => "- " + i).join("\n")}
Revise the summary. Address every issue. Same format as before.`
          : `Summarize the article below in at most 150 words, then give
exactly three key points.
ARTICLE:
${ctx.article}`,
        // NOTE: on revisit the article is NOT re-sent — it is already in the
        // adopted conversation. Only the feedback is appended.
      },
      output: {
        schema: {
          summary: "string",
          key_points: { type: "array", items: "string", minItems: 3, maxItems: 3 },
        },
      },
    },

    critique: {
      agent: {
        model: "ollama/llama3.2:3b", // fresh judge every round — deliberately NOT adopted,
        prompt: ({ ctx }) => `
You are a strict editor. Score the summary 0-10 for accuracy and
completeness against the article. List at most three concrete issues, each
under 20 words.
ARTICLE:
${ctx.article}
SUMMARY:
${ctx.draft.summary}`,
      },
      output: {
        schema: {
          score: "number",
          issues: { type: "array", items: "string", maxItems: 3 },
        },
        events: ["approve", "revise"],
      },
      transitions: [
        { on: "approve", when: ({ output }) => output.score >= 8, to: "publish" },
        { on: "revise", when: ({ visits }) => visits.draft < 3, to: "draft" },
        { to: "escalate" },
      ],
    },

    escalate: {
      human: {
        prompt: ({ ctx }) => `Revisions exhausted (last score ${ctx.critique.score}). Approve the current draft or fail the run?`,
        timeout: "1h",
        onTimeout: "failed",
      },
      transitions: [
        { on: "approved", to: "publish" },
        { on: "rejected", to: "failed" },
      ],
    },

    publish: {
      action: "file.write",
      input: {
        path: "out/summary.md",
        content: ({ ctx }) => `${ctx.draft.summary}

${ctx.draft.key_points.map(k => "- " + k).join("\n")}
`,
      },
    },
  },
};
