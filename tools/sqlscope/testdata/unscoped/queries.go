// Package unscoped is a fixture: one scoped table, one statement that names the pipeline and two that do not, plus const shapes the folder must survive: an implicit iota repeat (a name with no value of its own) and a cycle, which no compiling file has but this one is never compiled.
package unscoped

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
	pipeline_id INTEGER NOT NULL,
	hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS node_content (
	content_hash TEXT PRIMARY KEY
)`

const columns = `hash`

const (
	first = iota
	second
)

const (
	loopA = loopB
	loopB = loopA
)

var (
	_ = schema
	_ = `SELECT ` + columns + ` FROM nodes WHERE pipeline_id = ? AND hash = ?`
	_ = `SELECT COUNT(*) FROM nodes WHERE hash = ?`
	_ = `DELETE FROM ` + `nodes WHERE hash IN (` + placeholders() + `)`
	_ = `SELECT content_hash FROM node_content`
	_ = loopA
)

func placeholders() string { return "?" }
