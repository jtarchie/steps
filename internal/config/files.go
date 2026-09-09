package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Files is what a pipeline's includes are read through: a directory on disk, or the bundle a `steps pipeline set` uploaded, since a daemon has no sibling filesystem of the sender's.
type Files interface {
	ReadFile(name string) ([]byte, error)
}

// DirFS is a thin os.ReadFile rather than an fs.FS because fs.ValidPath refuses "..", and a shared ../tasks/ beside a pipelines/ directory is a legitimate layout at the same trust level as the pipeline file.
type DirFS string

// ReadFile reads name relative to the directory d wraps.
func (d DirFS) ReadFile(name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(string(d), name)) //nolint:gosec // trusted input, see DirFS
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return data, nil
}

// Bundle is CLOSED on purpose: a daemon resolving an include it was not sent must fail rather than read its own disk, or a crafted run_file: reads daemon-local files the sender never uploaded.
type Bundle map[string]string

// ReadFile cleans name first because filepath.Join cleans silently on disk, so `./ci/unit.sh` is recorded as ci/unit.sh and a sender keys its bundle off that form.
func (b Bundle) ReadFile(name string) ([]byte, error) {
	content, ok := b[filepath.Clean(name)]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, os.ErrNotExist)
	}

	return []byte(content), nil
}

// origin names where fsys reads from, for the "no such file" hint an author has to act on.
func origin(fsys Files) string {
	if dir, ok := fsys.(DirFS); ok {
		return fmt.Sprintf("the pipeline directory %q", string(dir))
	}

	return "the uploaded pipeline configuration"
}

// maxPipelineNameLength bounds a name that becomes a URL path segment and a database identity.
const maxPipelineNameLength = 200

// ValidPipelineName is an allow-list because the name is CONCATENATED into "/p/"+slug, where "?" truncates into a query, "%" decodes into a segment the name never had, and ".." traverses.
func ValidPipelineName(name string) error {
	if name == "" {
		return errors.New("pipeline name must not be empty")
	}

	if len(name) > maxPipelineNameLength {
		return fmt.Errorf("pipeline name must be %d characters or fewer", maxPipelineNameLength)
	}

	if name == "." || name == ".." {
		return fmt.Errorf("pipeline name %q is not valid", name)
	}

	for _, r := range name {
		if !validNameRune(r) {
			return fmt.Errorf("pipeline name %q must contain only letters, digits, %q, %q or %q", name, ".", "_", "-")
		}
	}

	return nil
}

func validNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-':
		return true
	default:
		return false
	}
}
