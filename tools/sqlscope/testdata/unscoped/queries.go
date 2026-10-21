// Package unscoped is a fixture: one scoped table, one statement that names the pipeline and two that do not, plus const shapes the folder must survive: an implicit iota repeat (a name with no value of its own) and a cycle, which no compiling file has but this one is never compiled.
package unscoped

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
	pipeline_id INTEGER NOT NULL,
	hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS node_content (
	content_hash TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS runs (
	id TEXT PRIMARY KEY,
	pipeline_id INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS run_steps (
	run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS late (
	id TEXT PRIMARY KEY
);
ALTER TABLE late ADD COLUMN pipeline_id INTEGER NOT NULL DEFAULT 0`

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
	_ = `INSERT INTO run_steps (run_id, step_name) VALUES (?, ?)`
	_ = `INSERT INTO run_steps (run_id, step_name) SELECT ?, ? WHERE EXISTS (SELECT 1 FROM runs WHERE id = ? AND pipeline_id = ?)`
	_ = `SELECT s.step_name FROM run_steps s JOIN runs r ON r.id = s.run_id WHERE r.pipeline_id = ?`
	_ = `DELETE FROM late WHERE id = ?`
)

func placeholders() string { return "?" }
