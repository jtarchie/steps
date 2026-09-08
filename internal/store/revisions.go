package store

// Revision is one recorded configuration: the substituted source a run was
// started from, and the hash that identifies it.
type Revision struct {
	SHA    string
	Source string
}
