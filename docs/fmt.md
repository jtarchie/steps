# steps fmt

`steps fmt` rewrites a pipeline file into one standard layout, so every pipeline reads the same way: top to bottom, with the important part of each block in a predictable place. It never changes what a pipeline does, and it never deletes or rewords a comment. There are no options and no configuration file — one format, the one every example in these docs is already in.

```
steps fmt pipeline.yml      rewrite the file in place (prints its path when it changed)
steps fmt ci/               every .yml/.yaml under the folder; a file that is not
                            a pipeline (a vars file, a task file) is skipped and named
steps fmt -                 read standard input, print the result (for editors)
steps fmt --check <paths>   change nothing; fail naming each file not formatted
steps fmt --diff <paths>    change nothing; print a unified diff of what would change
```

`steps validate` and `steps pipeline set` do **not** refuse an unformatted pipeline: a pipeline that runs is never rejected over whitespace. Put `steps fmt --check` in a pre-commit hook or a build if you want formatting enforced. Naming rules and "this resource is unused" style findings are not formatting; they stay with `steps validate`.

## The layout

- **Each block starts with what it is.** A step's verb (`get:`, `task:`, `agent:`, `do:`, …) comes first, then `name:`, then `type:`; then its settings in the order you wrote them; then its body (`plan:`, `system:`, `run:`, `messages:`); then `assert:`; then its hooks (`on_success:`, `on_failure:`, `on_error:`, `on_abort:`, `ensure:`, `on_pending:`) last. Which mappings are blocks is read from steps' own config types, so a key steps adds tomorrow is placed without a change here.
- **Your order is kept where order is your meaning.** Free-form maps (`source:`, `params:`, `vars:`, `env:`) are never reordered, the top-level sections stay where you put them (except a pipeline `assert:`, which goes last), and lists — jobs, steps, resources — are never sorted.
- **Lists are indented under their key** (`resources:`, then `  - name:`), two spaces a level.
- **Blank lines follow structure.** One blank line between top-level sections, and between jobs, resources and agents that take more than one line. Anywhere else a single blank line you left (to group the steps of a plan) is kept, and a run of them becomes one.
- **Text is left alone.** Prompts, scripts and every other value keep their spelling: never re-wrapped, re-quoted, or switched between `|` and `>`. `((var))` placeholders stay as written — nothing is substituted, so a pipeline formats without its vars.
- **Comments are left alone.** Never reworded or re-wrapped; they move with the thing they describe. A trailing comment keeps the column you aligned it to (or, where the deeper indentation leaves no room, the gap you left before it), and a comment line continuing it stays under it. Two places the formatter keeps your key order rather than separate a comment from its subject: keys whose trailing comments, on consecutive lines, read as one sentence, and a block whose last key has a comment written under it.
- **Short inline lists stay inline.** `tools: [read_file, write_file]` is not expanded, though the spacing inside the brackets is normalized (`{ a: 1 }` becomes `{a: 1}`).
- **Anchors and merges stay as written** (`&name`, `<<: *name`, `*name`), never expanded into copies.

A file written like this:

```yaml fragment
jobs:
- plan:
  - run: echo testing          # stands in for go test ./...
    task: unit
  name: test
  on_failure:
    task: page
    run: echo broke
  assert:
    execution: [unit]
    outcome: succeeded
```

comes out like this:

```yaml
jobs:
  - name: test
    plan:
      - task: unit
        run: echo testing      # stands in for go test ./...
    assert:
      execution: [unit]
      outcome: succeeded
    on_failure:
      task: page
      run: echo broke
```

## Guarantees

- **Meaning never changes.** Before writing, `steps fmt` decodes the original and the result and requires the same values; loads both through the same loader `steps run` uses and requires the same pipeline (a file that does not load, say for a var it was not given, must still not load); and requires every comment of the original to still be there. Any difference and the file is left untouched, with an error saying which check refused.
- **Running it twice changes nothing the second time.**
- **The docs are the style reference.** Every pipeline example in these pages and in `examples/` is already formatted; the test suite fails if one drifts.
