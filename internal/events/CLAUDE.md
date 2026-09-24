# internal/events

The stdlib-only leaf carrying run events from the execution packages to `internal/web`.

- **events must stay a leaf.** Every execution package publishes to it; the moment it imports config or store, they all inherit that edge and the graph stops being acyclic. `.golangci.yml`'s depguard allow-list is what enforces this — widening it is the failure mode, not a fix.

- **`StepID`/`ParentStepID` are the DISPLAY tree, minted per run — not the merkle chain.** Four things stop `nodes.parent_hash` from answering "what ran inside what": an `across:` cell hashes under the *matrix's* predecessor rather than under the matrix, so the block and its cells are siblings there; a `when:`-skipped step's own hash IS its parent's, which would graft it onto itself; a step's hash is not known when it *starts*, because `load_var` substitution rewrites the step inside the dispatch; and the chain is a *cache*, count-bounded and pruned, where a display tree must live exactly as long as its run. Ids are minted off the run's `resumeState` and scoped onto the context by the one step that runs children, so a container costs one line rather than a parameter through every runner.

- **An observer is not a subscriber, and the difference is loss.** `Subscribe` hands out a buffered channel that DROPS when full — right for a browser tab, which must never stall a run. `Observe` calls its func inline, in sequence order, and never drops — right for a terminal, where a dropped event is a line nobody ever sees. The cost is that a slow observer slows the run, which is what the `fmt.Printf` it replaced always did. Note the SINK drops too (a full `sinkBuffer`), so `run_events` is not lossless either; an observer is the only delivery that is.
