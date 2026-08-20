# internal/events

The stdlib-only leaf carrying run events from the execution packages to `internal/web`.

- **events must stay a leaf.** Every execution package publishes to it; the moment it imports config or store, they all inherit that edge and the graph stops being acyclic. `.golangci.yml`'s depguard allow-list is what enforces this — widening it is the failure mode, not a fix.

- **`StepID`/`ParentStepID` are the DISPLAY tree, minted per run — not the merkle chain.** Four things stop `nodes.parent_hash` from answering "what ran inside what": an `across:` cell hashes under the *matrix's* predecessor rather than under the matrix, so the block and its cells are siblings there; a `when:`-skipped step's own hash IS its parent's, which would graft it onto itself; a step's hash is not known when it *starts*, because `load_var` substitution rewrites the step inside the dispatch; and the chain is a *cache*, count-bounded and pruned, where a display tree must live exactly as long as its run. Ids are minted off the run's `resumeState` and scoped onto the context by the one step that runs children, so a container costs one line rather than a parameter through every runner.
