package config

import (
	"errors"
	"strings"
)

// Access narrows who may see and operate one pipeline on a daemon people
// sign in to. Each field is a rule, an expression over the signed-in person
// (see docs/authentication.md), and it can only NARROW what the daemon's own
// --role rules grant: a pipeline is a file anybody with a pull request can
// edit, so nothing written here can widen anyone's access. A daemon's admins
// are not narrowed — they can set or destroy the pipeline regardless.
//
// The rules are compiled by the daemon that serves the pipeline, which refuses
// the set if one cannot be; here only their shape is checked. A pipeline run
// with `steps run` has nobody signed in, and nothing to narrow.
//
// Never hashed: who may look at a build changes nothing it does.
type Access struct {
	// Viewer is who may see the pipeline at all. Anybody else is told it does not exist.
	Viewer string `yaml:"viewer,omitempty"`
	// Operator is who may trigger, approve and abort in it. Anybody else who may see it only reads.
	Operator string `yaml:"operator,omitempty"`
}

func (c *Config) validateAccess() error {
	if c.Access == nil {
		return nil
	}

	if strings.TrimSpace(c.Access.Viewer) == "" && strings.TrimSpace(c.Access.Operator) == "" {
		return errors.New("access: names neither viewer nor operator; remove it, or say who may see or operate this pipeline")
	}

	return nil
}
