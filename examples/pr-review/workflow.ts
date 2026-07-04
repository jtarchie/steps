/// <reference path="../../docs/src/global.d.ts" />
// Cheap scouts, expensive specialist. Small models triage each file and the
// PR as a whole; the large model only verifies flagged files with
// pre-gathered leads. Trivial PRs never reach it.
//
// States are plain consts; the flow at the bottom is the whole topology.
// Every computed value is a function of one flat scope — destructure what
// you need. Run `steps context workflow.js` to see what each state may use.

const allCalm = ({ scout_files }) => scout_files.items.every(i => i.risk === "low");
const clean = ({ deep_review }) => deep_review.items.every(i => i.findings.length === 0);

// diff.split is a builtin Go action: it parses one unified diff (the raw
// `gh pr diff` text) into a per-file work list the foreach below fans out
// over. Its output shape:
//
//   { files: [{ path, patch, additions, deletions, content? }], count }
//
//   - path/patch      — the file's name and its hunks from the diff
//   - additions/…     — change counts, cheap signal for prompts
//   - content         — ONLY when a `root` is given: the file's CURRENT text
//                       read from disk (confined to root, capped at
//                       context_bytes with a truncation marker), so scouts
//                       see the code AROUND the patch, not just the hunks
//
// `root` is the checkout directory the diff applies to (--input root=.).
// Without it, entries carry no `content` and the machine still works —
// scouts judge from patches alone. Pass by: "hunk" instead to split huge
// files into one entry per hunk.
const split_diff: State = {
  action: "diff.split",
  input: ({ diff, root }) => ({ diff, root: root || "", context_bytes: 3000 }),
};

// One hermetic micro-context per file. memo: unchanged files are free on
// re-review; skip: one unparseable file doesn't sink the PR.
const scout_files: State = {
  forEach: { over: ({ split_diff }) => split_diff.files, as: "file", concurrency: 3, onItemFailure: "skip" },
  memo: true,
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

// Same question, whole-PR altitude: sees stats and scout conclusions,
// never full patches.
const scout_pr: State = {
  memo: true,
  prompt: ({ title, description, split_diff, scout_files }) => `
    You are a scout for a senior code reviewer, looking at a pull request
    AS A WHOLE. Identify cross-file concerns the per-file scouts below
    cannot see: APIs changed without callers updated, docs promising what
    code retracts, missing tests. Then decide: does this PR need the senior
    at all? Choose event "trivial" only if nothing warrants review.
    ${title ? `PR: ${title} — ${description}` : ""}
    FILES:
    ${list(split_diff.files.map(f => `${f.path} (${f.additions}+/${f.deletions}-)`))}
    SCOUT REPORTS:
    ${list(scout_files.items.map(i => `${i.path} [${i.risk}]:${i.leads.map(l => " " + l.concern + ";").join("")}`))}`,
  output: {
    themes: { type: "array", maxItems: 4, items: "string" },
    reading_order: "string[]",
  },
  events: ["deep_review", "trivial"],
};

const note_trivial: State = {
  write: "out/review.md",
  content: ({ scout_pr }) => `## Automated triage: no senior review needed\n\n${list(scout_pr.themes)}\n`,
};

// The senior: only flagged files, one hermetic context per file, leads
// pre-gathered (the over mapper joins each lead with its patch). Verify or
// refute — never research. Medium risk routes to the small model; only
// high risk earns the big one.
const deep_review: State = {
  forEach: {
    over: ({ scout_files, split_diff }) => scout_files.items
      .filter(i => i.risk !== "low")
      .map(l => ({ ...l, patch: (split_diff.files.find(f => f.path === l.path) || {}).patch })),
    as: "lead",
  },
  memo: true,
  model: ({ lead }) => (lead.risk === "high" ? "senior" : "scout"),
  reasoning: "high",
  maxOutputTokens: 8192,
  maxTurns: 6, // room for on-demand file reads before the verdict
  tools: [
    // On-demand context, the complement of split_diff's enrichment: the
    // scout sees a CAPPED slice of each file; the senior may pull the FULL
    // file when the patch alone is not enough. The model decides WHEN, the
    // machine decides WHAT it may touch:
    //   - when:     only paths that are actually part of this PR
    //   - maxCalls: at most three reads per file under review
    //   - args:     `root` is machine-pinned (merged over the model's args,
    //               never overridable) — reads are resolved inside the
    //               checkout and path escapes like ../ are refused. This is
    //               the same `root` input split_diff used for enrichment.
    {
      name: "file.read",
      maxCalls: 3,
      when: ({ split_diff, args }) => split_diff.files.some(f => f.path === args.path),
      args: ({ root }) => ({ root: root || "" }),
    },
  ],
  prompt: ({ scout_pr, lead }) => `
    You are a senior reviewer. A scout flagged this file with specific
    leads. Verify or refute EACH lead against the patch, and report any
    defect the scout missed. Do not restate the diff. Report only what you
    can substantiate. If the patch alone is not enough context, read the
    full file with file_read before concluding.
    PR THEMES:${scout_pr.themes.map(t => " " + t + ";").join("")}
    FILE: ${lead.path}
    LEADS:
    ${list(lead.leads.map(l => `${l.where}: ${l.concern}`))}
    PATCH:
    ${lead.patch}`,
  output: {
    path: "string",
    findings: [{ where: "string", severity: "enum(blocking|important|nit)", issue: "string", fix: "string" }],
    leads_refuted: "string[]",
  },
};

const verdict: State = {
  memo: true,
  model: "senior",
  prompt: ({ deep_review }) => `
    Compose the review verdict from these substantiated findings. Be
    direct; credit refuted leads briefly.
    ${deep_review.items.map(i => `FILE ${i.path}:
    ${list(i.findings.map(f => `[${f.severity}] ${f.where}: ${f.issue} — fix: ${f.fix}`))}
    ${list(i.leads_refuted.map(r => "refuted: " + r))}`).join("\n")}`,
  output: { summary: "string", body: "string" },
  events: ["approve", "comment", "request_changes"],
};

const write_review: State = {
  write: "out/review.md", // swap for gh.post_review to publish for real
  content: ({ verdict }) => `## Code review\n\n${verdict.summary}\n\n${verdict.body}\n`,
};

export default {
  name: "pr-review",
  input: {
    diff: { type: "string", required: true }, // the unified diff text: gh pr diff 123 > pr.diff
    // Optional checkout root (--input root=.). Feeds BOTH context paths:
    // split_diff attaches each file's current text for the scouts
    // (deterministic, capped), and file.read is pinned to it so the
    // senior's on-demand reads cannot leave the checkout. Omit it and the
    // review runs on patches alone.
    root: "string",
    title: "string",
    description: "string",
  },
  models: {
    scout: "lmstudio/google/gemma-4-e4b", // small, local, effectively free
    senior: "lmstudio/google/gemma-4-26b-a4b", // larger, spent sparingly
  },
  model: "scout",
  defaults: { reasoning: "low" },
  limits: { maxTransitions: 15, maxTokens: 200000, timeout: "30m" }, // local models think slowly; the wall clock is enforced

  states: { split_diff, scout_files, scout_pr, note_trivial, deep_review, verdict, write_review },

  flow: pipe(
    split_diff,
    scout_files,
    branch(scout_pr, {
      // The scout proposes skipping the senior; the guard only allows it
      // when no file scout was worried. Agent proposes, guards dispose.
      trivial: when(allCalm).to(note_trivial),
      else: pipe(
        deep_review,
        branch(verdict, {
          // The senior cannot approve past substantiated findings.
          approve: when(clean).to(write_review),
          else: write_review,
        }),
      ),
    }),
  ),
};
