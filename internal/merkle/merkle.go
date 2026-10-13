// Package merkle plans a job's steps into content-addressed chains and
// computes the hashes used to skip already-succeeded work. The content-map
// builders here (GetNodeContent/TaskNodeContent/PutNodeContent/
// AgentContentMap) are shared between planning (this package) and real
// execution (internal/pipeline, internal/agent) so both compute identical
// hashes for identical steps.
package merkle

// NodeKind identifies which kind of plan step a Node represents.
type NodeKind string

// The NodeKind values. One a config.StepKind also names is spelled the same,
// which CellHash relies on when it hashes a cell under NodeKind(kind).
const (
	NodeKindGet   NodeKind = "get"
	NodeKindTask  NodeKind = "task"
	NodeKindPut   NodeKind = "put"
	NodeKindAgent NodeKind = "agent"
	NodeKindTry   NodeKind = "try"
	// NodeKindParallel is an in_parallel: block. The block hashes its
	// branches' content, so changing any branch changes the block.
	NodeKindParallel NodeKind = "in_parallel"
	// NodeKindRace is a race: block.
	NodeKindRace NodeKind = "race"
	// NodeKindDo is a do: block — steps run in sequence as one node.
	NodeKindDo NodeKind = "do"
	// NodeKindEnsemble is an ensemble: block.
	NodeKindEnsemble NodeKind = "ensemble"
	// NodeKindAcross is an across: matrix.
	NodeKindAcross NodeKind = "across"
	// NodeKindLoadVar is a load_var: capture.
	NodeKindLoadVar NodeKind = "load_var"
	// NodeKindApproval is an approval: wait.
	NodeKindApproval NodeKind = "approval"
)

// Node is one content-addressed step in a job's resolved execution chain.
// Its Hash folds in ParentHash, so Hash alone identifies the entire chain
// of steps that produced it.
type Node struct {
	Hash       string
	ParentHash string
	Kind       NodeKind
	StepIndex  int
	Resource   string // resource name (get/put) or task name (task); metadata only, not hashed
	Content    map[string]any
}

// InputSet is the versions one build binds, GET NAME to version — the unit a
// plan fans out over. Each every-mode get advances one step per set;
// everything else contributes its single resolved version to every set. It
// lives here because the planner and the executor must derive chains and
// builds from the SAME sets, and this package is where they already meet.
//
// Keyed by the get, not by the resource it fetches: Concourse resolves per
// INPUT, and two gets may read one resource on purpose — `get: code,
// version: every` beside `get: baseline, resource: code, version: {...}` is
// how a plan diffs what just arrived against a fixed point. A resource-keyed
// set gave them one shared binding, so the fan-out silently overrode the
// baseline's pin and the comparison always found nothing.
type InputSet map[string]map[string]any

// Chain is one root-to-leaf path through a job's plan: exactly what a
// triggered build's recursion walks today (a plain chain, not a general
// multi-parent DAG, since get: version: every is the only fan-out point).
type Chain struct {
	Nodes    []Node
	RootHash string
	// Unskippable is true if any node is a put step, an agent step, or a task
	// with a fix: — all have side effects or are non-deterministic (agent
	// runs, and a fix: task's success may depend on one) and must always run
	// rather than reuse a prior hash match.
	Unskippable bool
}
