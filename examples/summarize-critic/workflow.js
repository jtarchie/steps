// Draft a summary with a small local model, have a second micro-agent
// critique it, loop with feedback until it passes or a human decides.
// Context rung 1: each revision is a FRESH conversation re-primed with the
// article + distilled feedback. Compare ../summarize-critic-adopt/.

module.exports = {
  version: 1,
  name: "summarize-critic",

  input: { article: { type: "string", required: true } },

  defaults: {
    agent: {
      model: "ollama/qwen3:8b", // any OpenAI-compatible endpoint works
      maxTurns: 2, // tool-less states need one model call per turn; 2 is headroom
      reasoning: "low", // reasoning tokens are billed output; summarizing needs none
    },
  },

  limits: { maxTransitions: 12 }, // hard backstop for the revision loop

  states: {
    draft: {
      agent: {
        prompt: ({ ctx }) => `
Summarize the article below in at most 150 words, then give exactly three
key points.
${ctx.critique ? `A reviewer rejected your previous draft for these reasons:
${ctx.critique.issues.map(i => "- " + i).join("\n")}
Address every issue.` : ""}
ARTICLE:
${ctx.article}`,
      },
      output: {
        schema: {
          summary: "string",
          key_points: { type: "array", items: "string", minItems: 3, maxItems: 3 },
        },
      },
      // no transitions: linear default flows to critique
    },

    critique: {
      agent: {
        model: "ollama/llama3.2:3b", // different micro-agent, different model
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
          issues: { type: "array", items: "string", maxItems: 3 }, // schema doubles as an output budget
        },
        events: ["approve", "revise"],
      },
      transitions: [
        { on: "approve", when: ({ output }) => output.score >= 8, to: "publish" },
        { on: "revise", when: ({ visits }) => visits.draft < 3, to: "draft" }, // the ENGINE bounds the loop
        { to: "escalate" }, // fallback: guard veto, or revisions exhausted
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
      action: "file.write", // from the builtin tool library
      input: {
        path: "out/summary.md",
        content: ({ ctx }) => `${ctx.draft.summary}

${ctx.draft.key_points.map(k => "- " + k).join("\n")}
`,
      },
      // linear default: last state flows to the implicit done terminal
    },
  },
};
