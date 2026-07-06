/// <reference path="../../docs/src/global.d.ts" />
// Cheap scouts, expensive specialist. Small models triage each file and the
// PR as a whole; the large model only verifies flagged files with
// pre-gathered leads. Trivial PRs never reach it.
//
// ONE machine, two front doors. Locally you pass a diff
// (`--input diff=@fixtures/pr.diff`); as a GitHub webhook a `pull_request`
// event arrives and the machine fetches the diff itself. The seam is
// `fetch_pr` (gh.pr_diff): a supplied diff is echoed straight through (no gh
// call, so fixtures/CI stay hermetic), an empty one is fetched live. When a
// real PR is under review (a `pr` is present) the findings are posted back —
// inline comments, a summary comment, a commit check, and a label — otherwise
// the run just writes out/review.md.
//
// States are plain consts; the flow at the bottom is the whole topology.
// Every computed value is a function of one flat scope — destructure what
// you need. Run `steps context workflow.js` to see what each state may use.

const allCalm = ({ scout_files }) =>
  scout_files.items.every((i) => i.risk === "low");
const clean = ({ deep_review }) =>
  deep_review.items.every((i) => i.findings.length === 0);

// GitHub sends every pull_request action (opened, closed, edited, …); only a
// handful are worth a review, and drafts never are. The webhook map folds
// "draft" into the action, so this guard skips on payload data ALONE — no gh
// call, no model tokens — before the diff is even split. Local runs carry no
// action and always proceed.
const REVIEWABLE = ["opened", "synchronize", "reopened", "ready_for_review"];
const skipEvent = ({ action }) =>
  Boolean(action) && !REVIEWABLE.includes(action);

// The front door. gh.pr_diff PASSES A SUPPLIED DIFF THROUGH untouched (offline
// fixtures, `--input diff=@…`) and only shells out to `gh` when the diff is
// empty and a `pr` is given (webhook / `--input pr=123`). Either way the rest
// of the machine reads fetch_pr.diff — the two entry modes converge here.
const fetch_pr: State = {
  action: "gh.pr_diff",
  input: ({ pr, repo, diff, title, description }) => ({
    pr: pr || "",
    repo: repo || "",
    diff: diff || "",
    title: title || "",
    description: description || "",
  }),
  output: { diff: "string", title: "string", description: "string" },
};

// diff.split is a builtin Go action: it parses one unified diff into a per-file
// work list the foreach below fans out over. Its output shape:
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
  input: ({ fetch_pr, root }) => ({
    diff: fetch_pr.diff,
    root: root || "",
    context_bytes: 3000,
  }),
};

// One hermetic micro-context per file. memo: unchanged files are free on
// re-review; skip: one unparseable file doesn't sink the PR.
const scout_files: State = {
  forEach: {
    over: ({ split_diff }) => split_diff.files,
    as: "file",
    concurrency: 3,
    onItemFailure: "skip",
  },
  memo: true,
  prompt: ({ title, fetch_pr, file }) => `
    You are a scout for a senior code reviewer. Do NOT review. Identify what
    in this one file DESERVES the senior's attention, so they spend zero
    time researching: risky hunks, broken invariants, suspicious deletions.
    Echo the exact path. If nothing warrants attention, say risk low with
    no leads.
    ${(title || fetch_pr.title) ? "PR: " + (title || fetch_pr.title) : ""}
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
  prompt: ({ title, description, fetch_pr, split_diff, scout_files }) => `
    You are a scout for a senior code reviewer, looking at a pull request
    AS A WHOLE. Identify cross-file concerns the per-file scouts below
    cannot see: APIs changed without callers updated, docs promising what
    code retracts, missing tests. Then decide: does this PR need the senior
    at all? Choose event "trivial" only if nothing warrants review.
    ${
    (title || fetch_pr.title)
      ? `PR: ${title || fetch_pr.title} — ${
        description || fetch_pr.description
      }`
      : ""
  }
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

// The senior: only flagged files, one hermetic context per file, leads
// pre-gathered (the over mapper joins each lead with its patch). Verify or
// refute — never research. Medium risk routes to the small model; only
// high risk earns the big one.
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
    For each finding, give the NEW-file line number it sits on (the line as
    it appears in the "+" side of the patch) so it can be commented inline.
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
      line: "number", // new-file line, for inline review comments
      severity: "enum(blocking|important|nit)",
      issue: "string",
      fix: "string",
    }],
    leads_refuted: "string[]",
  },
};

const verdict: State = {
  memo: true,
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

// ---- publish (live PRs only) ------------------------------------------------
// Reached only when a real `pr` is present (fixture/offline runs route to done
// before here, so they never call gh). gh.pr_meta supplies the head SHA the
// commit check needs; gh.comment leaves the human-readable verdict; gh.status
// turns the guard's clean/dirty judgement into a green/red check.

const fetch_meta: State = {
  action: "gh.pr_meta",
  input: ({ pr, repo }) => ({ pr, repo: repo || "" }),
};

// One inline review comment per substantiated finding, at its file:line —
// gh.review_comment needs the PR head SHA (fetch_meta.headSha) as commit_id.
// retry:none + skip: a line GitHub rejects (422) is dropped, never fatal to
// the review. Empty when the senior substantiated nothing.
const post_inline: State = {
  forEach: {
    over: ({ deep_review }) =>
      deep_review.items.flatMap((i) =>
        i.findings.map((f) => ({
          path: i.path,
          line: f.line,
          body: `**[${f.severity}]** ${f.issue}\n\n_fix:_ ${f.fix}`,
        }))
      ),
    as: "finding",
    onItemFailure: "skip",
  },
  action: "gh.review_comment",
  retry: "none",
  input: ({ pr, repo, fetch_meta, finding }) => ({
    repo: repo || "",
    pr,
    commit_id: fetch_meta.headSha,
    path: finding.path,
    line: finding.line,
    body: finding.body,
  }),
};

const post_comment: State = {
  action: "gh.comment",
  input: ({ pr, repo, verdict }) => ({
    pr,
    repo: repo || "",
    body: `## Automated code review\n\n${verdict.summary}\n\n${verdict.body}`,
  }),
};

// The `clean` guard now has teeth: it decides whether the commit check is
// green or red. gh.status needs an explicit repo (webhook always supplies it).
const set_status: State = {
  action: "gh.status",
  input: ({ repo, fetch_meta, deep_review }) => ({
    repo: repo || "",
    sha: fetch_meta.headSha,
    state: deep_review.items.every((i) => i.findings.length === 0)
      ? "success"
      : "failure",
    context: "steps/pr-review",
    description: "Automated senior review",
  }),
};

// Tag the PR by outcome so a human can triage the queue at a glance.
const label_pr: State = {
  action: "gh.label",
  input: ({ pr, repo, deep_review }) => ({
    pr,
    repo: repo || "",
    add: [
      deep_review.items.some((i) => i.findings.length > 0)
        ? "changes-requested"
        : "reviewed",
    ],
  }),
};

const publish = pipe(
  fetch_meta,
  post_inline,
  post_comment,
  set_status,
  label_pr,
);

export default {
  name: "pr-review",
  input: {
    // Supply EITHER a diff (local/CI) OR a pr (+repo) — the webhook supplies
    // pr/repo/title/description/action, `steps run` supplies whichever you pass.
    pr: "string", // PR number/URL — from webhook.map or --input pr=123
    repo: "string", // owner/repo — required with pr for live comment/status
    diff: "string", // pre-fetched unified diff; when set, gh is NOT called
    title: "string",
    description: "string",
    // Optional checkout root (--input root=.). Feeds BOTH context paths:
    // split_diff attaches each file's current text for the scouts
    // (deterministic, capped), and file.read is pinned to it so the
    // senior's on-demand reads cannot leave the checkout. Omit it and the
    // review runs on patches alone.
    root: "string",
    action: "string", // webhook pull_request action (opened|synchronize|draft|…)
  },
  models: {
    scout: "lmstudio/google/gemma-4-e4b", // small, local, effectively free
    senior: "lmstudio/google/gemma-4-26b-a4b", // larger, spent sparingly
  },
  model: "scout",
  defaults: { reasoning: "low" },
  limits: { maxTransitions: 20, maxTokens: 200000, timeout: "30m" }, // local models think slowly; the wall clock is enforced

  // The trigger: `steps serve --hook workflow.ts` exposes POST /hooks/pr-review.
  // A GitHub `pull_request` webhook already carries the title/body and the PR
  // number — only the DIFF must be fetched (by fetch_pr). Drafts are folded
  // into `action` so skipEvent can bounce them with zero gh/model cost. Auth:
  // set the same secret as the GitHub webhook secret (HMAC X-Hub-Signature-256)
  // or pass it as ?token= in the payload URL.
  webhook: {
    path: "pr-review",
    map: ({ body }) => ({
      pr: String(body.number),
      repo: body.repository.full_name,
      title: body.pull_request.title,
      description: body.pull_request.body || "",
      action: (body.pull_request && body.pull_request.draft)
        ? "draft"
        : body.action,
    }),
  },

  states: {
    fetch_pr,
    split_diff,
    scout_files,
    scout_pr,
    note_trivial,
    deep_review,
    verdict,
    write_review,
    fetch_meta,
    post_inline,
    post_comment,
    set_status,
    label_pr,
  },

  flow: pipe(
    // Skip drafts and irrelevant webhook actions on payload data alone.
    branch(fetch_pr, [when(skipEvent).to(done)]),
    split_diff,
    scout_files,
    branch(scout_pr, {
      // The scout proposes skipping the senior; the guard only allows it
      // when no file scout was worried. Agent proposes, guards dispose.
      trivial: when(allCalm).to(note_trivial),
      else: pipe(
        deep_review,
        // The senior may propose "approve", but the guard only lets the
        // approve edge fire when nothing is substantiated; otherwise it
        // falls through to the same write — the veto is visible in the
        // journal (fired transition has an empty `on`).
        branch(verdict, {
          approve: when(clean).to(write_review),
        }),
        // Artifact always; comment + commit check only for a real PR.
        branch(write_review, [
          when(({ pr }) => Boolean(pr)).to(publish),
          done,
        ]),
      ),
    }),
  ),
};
