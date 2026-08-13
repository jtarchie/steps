# tools/kindswitch

The `go/analysis` pass in the root CLAUDE.md's validation sequence. It reads `config.Step`'s kind set out of `Step.Kind()`'s own table and publishes it as an analysis Fact, so **adding a kind to that table is the single edit that widens what it checks** — there is no second list to keep in sync. Its own fixtures live in `testdata/`.
