/// <reference path="../../docs/src/global.d.ts" />
// parallel-review — true concurrent fan-out/fan-in (fork/join).
//
// One change is reviewed from three independent angles AT THE SAME TIME, then a
// lead folds the three verdicts into a ship/no-ship call. `review` is a fork
// state: each label is a hermetic branch (its own sub-run, its own context),
// they run concurrently (bounded by `concurrency`), and the barrier joins them
// into a label-keyed aggregate the `verdict` state reads from scope.
//
// Like pr-review, it has two front doors: `fetch_pr` (gh.pr_diff) passes a
// supplied diff through untouched (fixtures / `--input diff=@…`) and only
// fetches live from a GitHub webhook `pull_request` event. When a real `pr` is
// present the ship decision is posted back as a PR comment.

const REVIEWABLE = ["opened", "synchronize", "reopened", "ready_for_review"];
const skipEvent = ({ action }) =>
  Boolean(action) && !REVIEWABLE.includes(action);

const fetch_pr: State = {
  action: "gh.pr_diff",
  input: ({ pr, repo, diff }) => ({
    pr: pr || "",
    repo: repo || "",
    diff: diff || "",
  }),
  output: { diff: "string", title: "string", description: "string" },
};

const security = {
  model: "reviewer",
  prompt: ({ fetch_pr }) =>
    `Security review — flag injection, authz, secrets, unsafe deserialization.\n\n${fetch_pr.diff}`,
  output: { risk: "enum(low, medium, high)", findings: "string[]" },
};

const performance = {
  model: "reviewer",
  prompt: ({ fetch_pr }) =>
    `Performance review — flag N+1 queries, unbounded loops, needless allocation.\n\n${fetch_pr.diff}`,
  output: { concerns: "string[]" },
};

const docs = {
  model: "reviewer",
  prompt: ({ fetch_pr }) =>
    `Docs review — flag public API changes that ship without doc/comment updates.\n\n${fetch_pr.diff}`,
  output: { gaps: "string[]" },
};

// The fork: three heterogeneous branches, run concurrently, joined at the
// barrier. Its scope entry becomes { security: {...}, performance: {...},
// docs: {...} } — read by label downstream.
const review = {
  parallel: { security, performance, docs },
  concurrency: 3,
  onBranchFailure: "fail", // one reviewer crashing fails the review
};

// The join: reads all three branch outputs by label and renders the verdict.
const verdict = {
  model: "lead",
  prompt: ({ review }) =>
    `A change was reviewed from three angles:
- security risk: ${review.security.risk}; findings: ${
      list(review.security.findings)
    }
- performance concerns: ${list(review.performance.concerns)}
- docs gaps: ${list(review.docs.gaps)}

Decide whether to ship. Block only on a HIGH security risk.`,
  output: { ship: "boolean", summary: "string" },
};

// Live PRs only: post the ship decision back as a comment.
const post_comment: State = {
  action: "gh.comment",
  input: ({ pr, repo, verdict }) => ({
    pr,
    repo: repo || "",
    body: `## Parallel review — ${
      verdict.ship ? "ship ✅" : "hold ⛔"
    }\n\n${verdict.summary}`,
  }),
};

export default {
  name: "parallel-review",
  input: {
    pr: "string", // webhook / --input pr=123
    repo: "string",
    diff: "string", // pre-fetched diff (fixtures/offline); when set, gh is NOT called
    action: "string", // webhook pull_request action
  },
  models: { reviewer: "mock", lead: "mock" },

  webhook: {
    path: "parallel-review",
    map: ({ body }) => ({
      pr: String(body.number),
      repo: body.repository.full_name,
      action: (body.pull_request && body.pull_request.draft)
        ? "draft"
        : body.action,
    }),
  },

  states: {
    fetch_pr,
    security,
    performance,
    docs,
    review,
    verdict,
    post_comment,
  },

  flow: pipe(
    branch(fetch_pr, [when(skipEvent).to(done)]),
    review,
    branch(verdict, [
      when(({ pr }) => Boolean(pr)).to(post_comment),
      done,
    ]),
  ),
};
