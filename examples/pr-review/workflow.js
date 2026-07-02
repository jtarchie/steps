// Cheap scouts, expensive specialist. Small models triage each file and the
// PR as a whole; the large model only verifies flagged files with
// pre-gathered leads. Trivial PRs never reach it.
//
// Structure is data; any computed value is a plain function of one scope
// argument ({ ctx, output, event, visits, run, ... }). Run
// `steps context workflow.js` to see what each state may reference.

module.exports = {
  version: 1,
  name: "pr-review",

  input: {
    diff: { type: "string", required: true }, // e.g. gh pr diff 123 > pr.diff
    root: { type: "string" }, // checkout root — enables file context beyond the diff
    title: { type: "string" },
    description: { type: "string" },
  },

  models: {
    scout: "lmstudio/google/gemma-4-e4b", // small, local, effectively free
    senior: "lmstudio/google/gemma-4-26b-a4b", // larger, spent sparingly
  },

  defaults: { agent: { model: "scout", reasoning: "low", maxTurns: 2 } },
  limits: { maxTransitions: 15, maxTokens: 200000 },

  states: {
    // With a root, each entry also carries the current file (capped) — scouts
    // see code around the patch, deterministically. add by: "hunk" for huge files.
    split_diff: {
      action: "diff.split",
      input: {
        diff: ({ ctx }) => ctx.diff,
        root: ({ ctx }) => ctx.root || "",
        context_bytes: 3000,
      },
    },

    // One hermetic micro-context per file. memo: unchanged files are free on
    // re-review; skip: one unparseable file doesn't sink the PR.
    scout_files: {
      forEach: { over: ({ ctx }) => ctx.split_diff.files, as: "file", concurrency: 3, onItemFailure: "skip" },
      memo: true,
      agent: {
        prompt: ({ ctx, file }) => `
You are a scout for a senior code reviewer. Do NOT review. Identify what in
this one file DESERVES the senior's attention, so they spend zero time
researching: risky hunks, broken invariants, suspicious deletions. Echo the
exact path. If nothing warrants attention, say risk low with no leads.
${ctx.title ? "PR: " + ctx.title : ""}
FILE ${file.path} (${file.additions}+ / ${file.deletions}-):
${file.patch}
${file.content ? "CURRENT FILE (for context):\n" + file.content : ""}`,
      },
      output: {
        schema: {
          path: "string",
          risk: "enum(low, medium, high)",
          leads: [{ where: "string", concern: "string" }],
        },
      },
    },

    // Same question, whole-PR altitude: sees stats and scout conclusions,
    // never full patches.
    scout_pr: {
      memo: true,
      agent: {
        prompt: ({ ctx }) => `
You are a scout for a senior code reviewer, looking at a pull request AS A
WHOLE. Identify cross-file concerns the per-file scouts below cannot see:
APIs changed without callers updated, docs promising what code retracts,
missing tests. Then decide: does this PR need the senior at all? Choose
event "trivial" only if nothing warrants review.
${ctx.title ? `PR: ${ctx.title} — ${ctx.description}` : ""}
FILES:
${ctx.split_diff.files.map(f => `- ${f.path} (${f.additions}+/${f.deletions}-)`).join("\n")}
SCOUT REPORTS:
${ctx.scout_files.items.map(i => `- ${i.path} [${i.risk}]:${i.leads.map(l => " " + l.concern + ";").join("")}`).join("\n")}`,
      },
      output: {
        schema: {
          themes: { type: "array", maxItems: 4, items: "string" },
          reading_order: "string[]",
        },
        events: ["deep_review", "trivial"],
      },
      transitions: [
        // The scout proposes skipping the senior; the guard only allows it
        // when no file scout was worried. Agent proposes, guards dispose.
        { on: "trivial", when: ({ ctx }) => ctx.scout_files.items.every(i => i.risk === "low"), to: "note_trivial" },
        { to: "deep_review" },
      ],
    },

    note_trivial: {
      action: "file.write",
      input: {
        path: "out/review.md",
        content: ({ ctx }) => `## Automated triage: no senior review needed

${ctx.scout_pr.themes.map(t => "- " + t).join("\n")}
`,
      },
      transitions: "done",
    },

    // The senior: only flagged files, one hermetic context per file, leads
    // pre-gathered. Verify or refute — never research. Medium-risk files
    // route to the small model; only high risk earns the big one.
    deep_review: {
      forEach: { over: ({ ctx }) => ctx.scout_files.items.filter(i => i.risk !== "low"), as: "lead" },
      memo: true,
      agent: {
        model: ({ lead }) => (lead.risk === "high" ? "senior" : "scout"),
        reasoning: "high",
        maxOutputTokens: 8192,
        maxTurns: 6, // room for on-demand file reads before the verdict
        tools: [
          // On-demand context: the senior decides WHEN it needs a full
          // file; the machine decides WHAT it may touch — only files in
          // this PR, at most three reads, rooted where the machine says.
          {
            name: "file.read",
            maxCalls: 3,
            when: ({ ctx, args }) => ctx.split_diff.files.some(f => f.path === args.path),
            args: ({ ctx }) => ({ root: ctx.root || "" }),
          },
        ],
        prompt: ({ ctx, lead }) => `
You are a senior reviewer. A scout flagged this file with specific leads.
Verify or refute EACH lead against the patch, and report any defect the
scout missed. Do not restate the diff. Report only what you can
substantiate. If the patch alone is not enough context, read the full file
with file_read before concluding.
PR THEMES:${ctx.scout_pr.themes.map(t => " " + t + ";").join("")}
FILE: ${lead.path}
LEADS:
${lead.leads.map(l => `- ${l.where}: ${l.concern}`).join("\n")}
PATCH:
${(ctx.split_diff.files.find(f => f.path === lead.path) || {}).patch}`,
      },
      output: {
        schema: {
          path: "string",
          findings: [{ where: "string", severity: "enum(blocking, important, nit)", issue: "string", fix: "string" }],
          leads_refuted: "string[]",
        },
      },
    },

    verdict: {
      memo: true,
      agent: {
        model: "senior",
        prompt: ({ ctx }) => `
Compose the review verdict from these substantiated findings. Be direct;
credit refuted leads briefly.
${ctx.deep_review.items.map(i => `FILE ${i.path}:
${i.findings.map(f => `- [${f.severity}] ${f.where}: ${f.issue} — fix: ${f.fix}`).join("\n")}
${i.leads_refuted.map(r => "- refuted: " + r).join("\n")}`).join("\n")}`,
      },
      output: {
        schema: { summary: "string", body: "string" },
        events: ["approve", "comment", "request_changes"],
      },
      transitions: [
        // The senior cannot approve past substantiated findings.
        { on: "approve", when: ({ ctx }) => ctx.deep_review.items.every(i => i.findings.length === 0), to: "write_review" },
        { to: "write_review" },
      ],
    },

    write_review: {
      action: "file.write", // swap for gh.post_review to publish for real
      input: {
        path: "out/review.md",
        content: ({ ctx }) => `## Code review

${ctx.verdict.summary}

${ctx.verdict.body}
`,
      },
    },
  },
};
