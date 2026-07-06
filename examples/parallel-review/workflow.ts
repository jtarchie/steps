// parallel-review — true concurrent fan-out/fan-in (fork/join).
//
// One change is reviewed from three independent angles AT THE SAME TIME, then a
// lead folds the three verdicts into a ship/no-ship call. `review` is a fork
// state: each label is a hermetic branch (its own sub-run, its own context),
// they run concurrently (bounded by `concurrency`), and the barrier joins them
// into a label-keyed aggregate the `verdict` state reads from scope.

const security = {
  model: "reviewer",
  prompt: ({ change }) =>
    `Security review — flag injection, authz, secrets, unsafe deserialization.\n\n${change}`,
  output: { risk: "enum(low, medium, high)", findings: "string[]" },
};

const performance = {
  model: "reviewer",
  prompt: ({ change }) =>
    `Performance review — flag N+1 queries, unbounded loops, needless allocation.\n\n${change}`,
  output: { concerns: "string[]" },
};

const docs = {
  model: "reviewer",
  prompt: ({ change }) =>
    `Docs review — flag public API changes that ship without doc/comment updates.\n\n${change}`,
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
  prompt: ({ review }) => `A change was reviewed from three angles:
- security risk: ${review.security.risk}; findings: ${list(review.security.findings)}
- performance concerns: ${list(review.performance.concerns)}
- docs gaps: ${list(review.docs.gaps)}

Decide whether to ship. Block only on a HIGH security risk.`,
  output: { ship: "boolean", summary: "string" },
};

export default {
  name: "parallel-review",
  input: { change: { type: "string", required: true } },
  models: { reviewer: "mock", lead: "mock" },
  states: { security, performance, docs, review, verdict },
  flow: pipe(review, verdict, done),
};
