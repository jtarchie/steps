// Package wire carries a step's work across a venue: the framed protocol an
// orchestrator and a pushed shim speak, and the codec that moves a step's
// directory tree between them.
//
// It is stdlib-only on purpose. Both ends of a venue run a binary the
// orchestrator itself pushed, so the two must agree on these bytes exactly;
// a dependency only one side has is a way for them to stop agreeing.
package wire
