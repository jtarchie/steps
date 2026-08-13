# examples/

The runnable example corpus lives in `docs/*.md` — every fenced ```yaml block there is a complete pipeline, extracted and executed by the root package's `TestDocsExamples` (see `docs_test.go`). This directory keeps the two things that can't be doc blocks:

**`pr-review.yml`** is the capstone example: the full adaptive PR-review pipeline (planner-decided matrix width, concurrent reviewer cells, collected outputs, synthesizer fan-in, human approval before posting), meant to be run against a real model and a real repo. It can never be a deterministic fixture — its pass/fail depends on what the reviewers find — so it is validated statically by `TestValidatePRReviewExample` (schema + full `steps validate`), and its deterministic *shape* is pinned by `e2e_pr_review_test.go` against the fake provider. Keep all three in sync when editing it.

**`invalid/*.yml` are pipelines that must FAIL `LoadConfig`**, each with a `# expect: <substring>` line naming the error it has to produce (`examples_invalid_test.go`). That is where a load-time rejection gets a readable file, rather than living only as an error-substring assertion in a Go test.
