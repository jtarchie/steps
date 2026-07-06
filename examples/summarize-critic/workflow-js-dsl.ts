/// <reference path="../../docs/src/global.d.ts" />
// The aasm-style whole-machine twin of ./workflow.ts — the SAME draft/critique
// loop, authored by CALLING verbs on a builder (m.state / m.event / m.always)
// top-to-bottom, the way you'd write `aasm do ... end`. It lowers to the same
// enforced graph and runs against the same mock_responses.yaml, producing the
// exact trace of the object form (TestSummarizeCriticJSDSLParity).
//
// The topology is EVENT-CENTRIC: named events own their from->to(+guard) edges.
// Two things read differently from workflow.ts's loop()/branch():
//   - The bounded loop is explicit — `revise` is just an event guarded by
//     `visits.critique < 3`, and the exhausted route is the trailing `always`.
//   - Events are declared by USE: m.event("approve", { from: critique, ... })
//     auto-adds "approve" to critique's output.events, so there is no separate
//     events: [...] line to keep in sync (human gates are skipped — they route
//     on resume events).
// Everything under the hood is the same primitive: m.state wraps the state()
// builder, and every edge lowers to an ordinary State.Transition. The classic
// object + flow form in workflow.ts still works unchanged; this is just sugar.

export default machine("summarize-critic", (m) => {
  m.needs({ article: "string" });
  m.model("ollama/qwen3:8b");
  m.defaults({ reasoning: "low" });
  m.limit({ maxTransitions: 12 });

  const draft = m.state("draft", (s) =>
    s
      .prompt(({ article, critique }) => `
        Summarize the article below in at most 150 words, then give exactly
        three key points.
        ${
        critique
          ? "A reviewer rejected your previous draft for these reasons:\n" +
            list(critique.issues) + "\nAddress every issue."
          : ""
      }
        ARTICLE:
        ${article}`)
      .output({
        summary: "string",
        key_points: {
          type: "array",
          items: "string",
          minItems: 3,
          maxItems: 3,
        },
      }));

  const critique = m.state("critique", (s) =>
    s
      .model("ollama/llama3.2:3b")
      .prompt(({ article, draft }) => `
        You are a strict editor. Score the summary 0-10 for accuracy and
        completeness against the article. List at most three concrete issues,
        each under 20 words.
        ARTICLE:
        ${article}
        SUMMARY:
        ${draft.summary}`)
      .output({
        score: "number",
        issues: { type: "array", items: "string", maxItems: 3 },
      }));
  // no .events(...) — m.event below auto-declares "approve" and "revise"

  const escalate = m.state("escalate", (s) =>
    s
      .human(({ critique }) =>
        `Revisions exhausted (last score ${critique.score}). Approve the current draft or fail the run?`
      )
      .choices({
        approved: "Ship the current draft as-is",
        rejected: "Fail the run",
      })
      .timeout("1h"));

  const publish = m.state("publish", (s) =>
    s
      .write("out/summary.md")
      .content(({ draft }) =>
        `${draft.summary}\n\n${list(draft.key_points)}\n`
      ));

  // The whole topology, as a sentence of events:
  m.start(draft);
  m.step(draft, critique); //                       draft always advances to the critic
  m.event("approve", {
    from: critique,
    to: publish,
    when: ({ output }) => output.score >= 8,
  });
  m.event("revise", {
    from: critique,
    to: draft,
    when: ({ visits }) => visits.critique < 3,
  });
  m.always(critique, escalate); //                  budget spent -> a human decides
  m.event("approved", { from: escalate, to: publish });
  m.event("rejected", { from: escalate, to: fail });
  m.timeout(escalate, fail);
});
