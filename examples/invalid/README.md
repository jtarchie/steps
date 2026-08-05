# examples/invalid — pipelines that must be rejected

Every file here is a **deliberately broken pipeline**. `TestExamplesInvalid`
(`../../examples_invalid_test.go`) requires each one to fail `config.LoadConfig`
with the error named in its `# expect:` line.

Why the directory exists: `examples/*.yml` is globbed by four tests that all
require a file to load, validate against `steps.schema.json`, and run green. A
pipeline whose whole point is to be rejected can never live there. So every
load-time rejection `steps` implements — and there are a lot of them — used to
exist only as an error-substring assertion inside a Go test, with no file a
human could read.

## Adding one

1. Write the smallest pipeline that triggers the rejection.
2. Add `# expect: <substring>` on its own comment line. The substring is matched
   against the error `LoadConfig` returns.
3. That's it — the glob picks it up.

The `# expect:` line is the load-bearing part. Asserting only "it failed" would
pass on a typo in the pipeline, proving nothing about the rule under test.

## What this directory is not

It is not a replacement for the Go table tests in `internal/config`. Those stay:
they're faster, they can assert on a rejection that has no readable YAML form,
and they run without the root package. This directory is the *demonstrable* half
— the file `docs/` can link to when it says "this is rejected".
