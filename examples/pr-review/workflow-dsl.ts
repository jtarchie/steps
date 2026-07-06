/// <reference path="../../docs/src/global.d.ts" />
// The higher-level-DSL twin of ./workflow.ts — same triage machine, showing
// the model-tier sugar. The scout/senior tiers now bundle the per-role knobs
// (here: memo), so scout_files/scout_pr/verdict stop restating memo: true.
// deep_review keeps its knobs explicit ON PURPOSE: its model is a per-item
// routing function, so no static tier applies — the one case tiers can't
// fold. Runs against the same mock_responses.yaml.

const allCalm = ({ scout_files }) =>
  scout_files.items.every((i) => i.risk === "low");
const clean = ({ deep_review }) =>
  deep_review.items.every((i) => i.findings.length === 0);

const split_diff: State = {
  action: "diff.split",
  input: ({ diff, root }) => ({ diff, root: root || "", context_bytes: 3000 }),
};

// memo now comes from the scout tier.
const scout_files: State = {
  forEach: {
    over: ({ split_diff }) => split_diff.files,
    as: "file",
    concurrency: 3,
    onItemFailure: "skip",
  },
  prompt: ({ title, file }) => `
    You are a scout for a senior code reviewer. Do NOT review. Identify what
    in this one file DESERVES the senior's attention, so they spend zero
    time researching: risky hunks, broken invariants, suspicious deletions.
    Echo the exact path. If nothing warrants attention, say risk low with
    no leads.
    ${title ? "PR: " + title : ""}
    FILE ${file.path} (${file.additions}+ / ${file.deletions}-):
    ${file.patch}
    ${file.content ? "CURRENT FILE (for context):\n" + file.content : ""}`,
  output: {
    path: "string",
    risk: "enum(low, medium, high)",
    leads: [{ where: "string", concern: "string" }],
  },
};

const scout_pr: State = {
  prompt: ({ title, description, split_diff, scout_files }) => `
    You are a scout for a senior code reviewer, looking at a pull request
    AS A WHOLE. Identify cross-file concerns the per-file scouts below
    cannot see: APIs changed without callers updated, docs promising what
    code retracts, missing tests. Then decide: does this PR need the senior
    at all? Choose event "trivial" only if nothing warrants review.
    ${title ? `PR: ${title} — ${description}` : ""}
    FILES:
    ${
    list(split_diff.files.map((f) =>
      `${f.path} (${f.additions}+/${f.deletions}-)`
    ))
  }
    SCOUT REPORTS:
    ${
    list(scout_files.items.map((i) =>
      `${i.path} [${i.risk}]:${
        i.leads.map((l) =>
          " " + l.concern + ";"
        ).join("")
      }`
    ))
  }`,
  output: {
    themes: { type: "array", maxItems: 4, items: "string" },
    reading_order: "string[]",
  },
  events: ["deep_review", "trivial"],
};

const note_trivial: State = {
  write: "out/review.md",
  content: ({ scout_pr }) =>
    `## Automated triage: no senior review needed\n\n${
      list(scout_pr.themes)
    }\n`,
};

// A per-item routing function picks the model, so no static tier applies —
// this state keeps memo/reasoning/maxOutputTokens explicit.
const deep_review: State = {
  forEach: {
    over: ({ scout_files, split_diff }) =>
      scout_files.items
        .filter((i) => i.risk !== "low")
        .map((l) => ({
          ...l,
          patch: (split_diff.files.find((f) => f.path === l.path) || {}).patch,
        })),
    as: "lead",
  },
  memo: true,
  model: ({ lead }) => (lead.risk === "high" ? "senior" : "scout"),
  reasoning: "high",
  maxOutputTokens: 8192,
  maxTurns: 6,
  tools: [
    {
      name: "file.read",
      maxCalls: 3,
      when: ({ split_diff, args }) =>
        split_diff.files.some((f) => f.path === args.path),
      args: ({ root }) => ({ root: root || "" }),
    },
  ],
  prompt: ({ scout_pr, lead }) => `
    You are a senior reviewer. A scout flagged this file with specific
    leads. Verify or refute EACH lead against the patch, and report any
    defect the scout missed. Do not restate the diff. Report only what you
    can substantiate. If the patch alone is not enough context, read the
    full file with file_read before concluding.
    PR THEMES:${scout_pr.themes.map((t) => " " + t + ";").join("")}
    FILE: ${lead.path}
    LEADS:
    ${list(lead.leads.map((l) => `${l.where}: ${l.concern}`))}
    PATCH:
    ${lead.patch}`,
  output: {
    path: "string",
    findings: [{
      where: "string",
      severity: "enum(blocking|important|nit)",
      issue: "string",
      fix: "string",
    }],
    leads_refuted: "string[]",
  },
};

// memo now comes from the senior tier.
const verdict: State = {
  model: "senior",
  prompt: ({ deep_review }) => `
    Compose the review verdict from these substantiated findings. Be
    direct; credit refuted leads briefly.
    ${
    deep_review.items.map((i) =>
      `FILE ${i.path}:
    ${
        list(i.findings.map((f) =>
          `[${f.severity}] ${f.where}: ${f.issue} — fix: ${f.fix}`
        ))
      }
    ${
        list(i.leads_refuted.map((r) => "refuted: " + r))
      }`
    ).join("\n")
  }`,
  output: { summary: "string", body: "string" },
  events: ["approve", "comment", "request_changes"],
};

const write_review: State = {
  write: "out/review.md",
  content: ({ verdict }) =>
    `## Code review\n\n${verdict.summary}\n\n${verdict.body}\n`,
};

export default {
  name: "pr-review",
  input: {
    diff: { type: "string", required: true },
    root: "string",
    title: "string",
    description: "string",
  },
  models: {
    // Tiers now carry memo — the scouts and the senior are all replay-safe.
    scout: { model: "lmstudio/google/gemma-4-e4b", memo: true },
    senior: { model: "lmstudio/google/gemma-4-26b-a4b", memo: true },
  },
  model: "scout",
  defaults: { reasoning: "low" },
  limits: { maxTransitions: 15, maxTokens: 200000, timeout: "30m" },

  states: {
    split_diff,
    scout_files,
    scout_pr,
    note_trivial,
    deep_review,
    verdict,
    write_review,
  },

  flow: pipe(
    split_diff,
    scout_files,
    branch(scout_pr, {
      trivial: when(allCalm).to(note_trivial),
      else: pipe(
        deep_review,
        branch(verdict, {
          approve: when(clean).to(write_review),
          else: write_review,
        }),
      ),
    }),
  ),
};
