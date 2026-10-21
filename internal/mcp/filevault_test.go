package mcp

// fileVault keeps logins where steps used to, so the tests written against that layout go on describing behavior rather than storage; the store's own suite and internal/cli test the sealed vault production installs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type fileVault struct{}

func (fileVault) Load(_ context.Context, pipeline, server string) ([]byte, string, bool, error) {
	path, err := TokenPath(pipeline, server)
	if err != nil {
		return nil, "", false, err
	}

	data, err := os.ReadFile(path) //nolint:gosec // TokenPath's own output
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", false, nil
	}

	if err != nil {
		return nil, "", false, fmt.Errorf("read %s: %w", path, err)
	}

	return data, "", true, nil
}

func (fileVault) Save(_ context.Context, pipeline, server, _ string, data []byte) error {
	path, err := TokenPath(pipeline, server)
	if err != nil {
		return err
	}

	var tf TokenFile

	err = json.Unmarshal(data, &tf)
	if err != nil {
		return fmt.Errorf("parse login: %w", err)
	}

	return tf.Save(path)
}

// Save writes t to path atomically (temp file in the same directory, then
// rename), so a concurrent reader — e.g. another `steps web
// --max-concurrent` worker refreshing the same server's token — never
// observes a half-written file. File permissions are 0600 (dir 0700).
// Only a login calls it: it is the one writer allowed to create the
// pipeline's directory.
func (t *TokenFile) Save(path string) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}

	return t.replace(path)
}

// replace is Save without creating the directory, for a refresh write-back:
// after a rename or destroy moved the directory away, recreating it would
// leave a live refresh token under a name a later pipeline inherits.
func (t *TokenFile) replace(path string) error {
	dir := filepath.Dir(path)

	data, err := json.MarshalIndent(t, "", "  ") //nolint:gosec // deliberate: this whole file's job is persisting these secrets to a 0600, non-merkle-hashed, per-pipeline token file — see TokenFile's doc comment
	if err != nil {
		return fmt.Errorf("marshal token file: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}

	tmpPath := tmp.Name()

	err = saveTemp(tmp, data, tmpPath)
	if err != nil {
		return err
	}

	err = os.Chmod(tmpPath, 0o600)
	if err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("chmod temp token file: %w", err)
	}

	err = os.Rename(tmpPath, path)
	if err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("rename temp token file: %w", err)
	}

	return nil
}

// saveTemp writes data to the already-created tmp file and closes it,
// cleaning up on any failure — split out of Save to keep it simple.
func saveTemp(tmp *os.File, data []byte, tmpPath string) error {
	_, writeErr := tmp.Write(data)

	closeErr := tmp.Close()
	if writeErr != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("write temp token file: %w", writeErr)
	}

	if closeErr != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("close temp token file: %w", closeErr)
	}

	return nil
}
