package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Cache is the merkle skip index — the nodes and job_runs a rerun consults to
// decide what it does not have to do again — plus the transcript a cached
// agent step is still readable through.
//
// Bounded by COUNT and never by age: a fully-cached poll records no new node
// and refreshes no timestamp, so an age floor sweeps past a working
// pipeline's cache, the faster it polls the sooner it loses it.
type Cache interface {
	// RecordChainSucceeded adds a whole chain to the skip index.
	// ForgetChain removes it: a chain that ran again and failed — a --force
	// rerun, a hook — must not be skipped on the strength of the older
	// success. Failure is a removal rather than a row because the index
	// holds only what a rerun may skip; a failed run's classification lives
	// on its run and its nodes, and a job failing through fresh content
	// every time must not be able to crowd the green chains out of a
	// count-capped table.
	RecordChainSucceeded(ctx context.Context, jobName, rootHash string) error
	ForgetChain(ctx context.Context, jobName, rootHash string) error
	HasSucceededBatch(ctx context.Context, jobName string, rootHashes []string) (map[string]bool, error)
	RecordNode(ctx context.Context, node NodeRecord, jobName, status string, result map[string]any, execErr error) error
	HasNodeSucceeded(ctx context.Context, jobName, hash string) (bool, error)
	ListNodes(ctx context.Context, jobName string, limit int) ([]NodeRow, error)
	NodesByHash(ctx context.Context, hashes []string) (map[string]NodeRow, error)
	// SaveNodeTranscript replaces the transcript of a node, truncated to
	// MaxTranscriptBytes along whole events so it stays valid JSON.
	SaveNodeTranscript(ctx context.Context, hash, transcript string) error
	NodeTranscript(ctx context.Context, hash string) (string, bool, error)
}

// NodeRecord is the subset of a merkle plan node's fields a driver persists.
// It's a plain data shape rather than an import of merkle.Node so this leaf
// package doesn't need to depend on the planner — callers convert their own
// Node type into one of these.
type NodeRecord struct {
	Hash       string
	ParentHash string
	Kind       string
	StepIndex  int
	Resource   string // resource name (get/put) or task name (task); metadata only
	Content    map[string]any
}

// NodeRow is one recorded step, with whatever the step produced.
type NodeRow struct {
	Hash      string
	Kind      string
	JobName   string
	Resource  string
	StepIndex int
	Status    string
	Error     string
	Result    string
	CreatedAt time.Time
	// Content and ParentHash are what the hash is MADE of — populated by
	// NodesByHash (the node-detail read), left empty by the list queries,
	// whose callers want a table row rather than a whole content map.
	Content    string
	ParentHash string
}

// MaxTranscriptBytes bounds one stored transcript.
//
// internal/agent already caps a single tool RESULT (maxRecordedResultBytes),
// which bounds the widest single value in a transcript but not the transcript:
// a conversation has as many turns as it needs, and an agent that worked for an
// hour writes all of them into one row. The largest value in the schema was
// therefore the one with no total bound at all.
//
// 256KB is far past any transcript worth reading end to end while staying an
// order of magnitude below the point where one row dominates the database. A
// transcript is a diagnostic, not a ledger: nothing reads it to make a
// decision, so losing the tail of a very long one costs nothing that the
// per-step token counts in agent_usage do not still record exactly.
const MaxTranscriptBytes = 256 * 1024

// ContentKey is the interning key for node_content: a hash OF the content
// bytes.
//
// Its own hash rather than the node's, because a node hash folds in its parent
// — two byte-identical steps in different chains must share one row, and
// under a node hash they could not.
func ContentKey(content []byte) string {
	sum := sha256.Sum256(content)

	return hex.EncodeToString(sum[:])
}

// TruncationEvent is appended in place of the entries dropped at the cap, so a
// reader who reaches the end knows the conversation continued rather than
// stopped. Spelled as a transcript event because that is what the consumers
// decode (internal/agent's transcriptEvent, rendered by internal/web).
const TruncationEvent = `{"type":"text","text":"[transcript truncated: over the stored size limit]"}`

// TruncateTranscript cuts a transcript to at most MaxTranscriptBytes while
// keeping it VALID JSON, by dropping whole events off the end.
//
// The first version sliced the string at a byte offset and appended a plain-text
// notice. That produced `[{"type":"text","text":"ttt` + a bare newline — invalid
// JSON, so json.Unmarshal in the renderer failed and every over-cap transcript
// displayed as NO transcript at all, the exact opposite of keeping the head. It
// passed a test asserting only that the stored string was non-empty. Byte
// slicing could also cut a multi-byte rune in half.
//
// Events are dropped from the TAIL: the task, the first tool calls and the early
// decisions are what explain what a step was doing, and a conversation long
// enough to hit the cap is one whose middle is mostly repetition.
//
// Anything that does not parse as a JSON array is passed through untouched under
// a hard byte cap. This function's job is a storage bound, not validation — a
// caller that hands over something else has a bug of its own, and silently
// rewriting it would hide that.
func TruncateTranscript(transcript string) string {
	if len(transcript) <= MaxTranscriptBytes {
		return transcript
	}

	var events []json.RawMessage

	err := json.Unmarshal([]byte(transcript), &events)
	if err != nil || len(events) == 0 {
		return TruncateUTF8(transcript, MaxTranscriptBytes)
	}

	// Two brackets, the notice, and the comma before it.
	budget := MaxTranscriptBytes - len(TruncationEvent) - len(`[,]`)

	kept, used := 0, 0

	for _, event := range events {
		// Each event costs its own bytes plus the comma joining it to the last.
		cost := len(event)
		if kept > 0 {
			cost++
		}

		if used+cost > budget {
			break
		}

		used += cost
		kept++
	}

	parts := make([]string, 0, kept+1)
	for _, event := range events[:kept] {
		parts = append(parts, string(event))
	}

	parts = append(parts, TruncationEvent)

	return "[" + strings.Join(parts, ",") + "]"
}
