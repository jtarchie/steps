package pipeline

// Reading an across: axis's values out of a file an earlier step produced —
// the run-time half of `from_file:` (see config.AcrossVar).
//
// The values live in an ordinary artifact, so this reads them the way any
// step reads an input: materialize the artifact into a workspace and open the
// file. There is no store, and nothing the producing step had to opt into
// beyond declaring the output it was already declaring.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/workspace"
)

// maxAcrossFileBytes caps the file an axis is read from. The array is one step
// deciding the shape of the next, not a data transfer: a list long enough to
// approach this is one nobody meant to fan out over, and reading it into
// memory to say so would be the failure it is meant to prevent.
const maxAcrossFileBytes = 1 << 20 // 1 MiB

// resolveFileAxes reads every from_file: axis of step, returning the values
// each took this run. Empty for a matrix with no file axis — the common case,
// which never touches the workspace, since the loop below skips every static
// axis.
func resolveFileAxes(ctx context.Context, label string, step config.Step, bw workspace.BuildWorkspace) (map[string][]string, error) {
	values := map[string][]string{}

	for _, axis := range step.Across {
		if !axis.Runtime() {
			continue
		}

		items, err := readAcrossFile(ctx, axis, bw)
		if err != nil {
			return nil, fmt.Errorf("%s: across var %q: %w", label, axis.Var, err)
		}

		slog.Debug("across.from_file", "var", axis.Var, "file", axis.FromFile, "items", len(items))

		values[axis.Var] = items
	}

	return values, nil
}

// readAcrossFile materializes the artifact holding one axis's file and decodes
// it into the values that axis takes.
//
// The space is its own, and discarded: this reads a file, it does not run the
// step. Under the shared strategy that space IS the build root and the
// artifact is already there; under copy/btrfs it is materialized for the read
// and released — either way the caller needs to know neither.
func readAcrossFile(ctx context.Context, axis config.AcrossVar, bw workspace.BuildWorkspace) ([]string, error) {
	artifact := axis.SourceArtifact()
	label := "across-" + axis.Var

	space, err := bw.TaskSpace(ctx, label, []string{artifact}, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	defer workspace.CloseSpace(space, label)

	// The path is confined at load (checkFromFilePath rejects absolute paths
	// and any ".." that climbs out), so this cannot leave the space.
	full := filepath.Join(space.Dir(), filepath.FromSlash(axis.FromFile))

	info, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w — the step that produces %q must write it before this block runs", axis.FromFile, err, artifact)
	}

	if info.Size() > maxAcrossFileBytes {
		return nil, fmt.Errorf("%s is %d bytes, above the limit of %d", axis.FromFile, info.Size(), maxAcrossFileBytes)
	}

	data, err := os.ReadFile(full) //nolint:gosec // path confined at load by checkFromFilePath, joined under this step's own materialized space
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", axis.FromFile, err)
	}

	return decodeAcrossItems(axis.FromFile, data)
}

// decodeAcrossItems turns the file's bytes into an axis's values, refusing
// every shape that is not a JSON array of strings.
//
// Loudly, and naming the file: the array is produced during the run, often by
// a model, so a wrong shape here is the most likely failure and the one whose
// message has to say what was found rather than what a decoder expected.
func decodeAcrossItems(name string, data []byte) ([]string, error) {
	var items []string

	err := json.Unmarshal(data, &items)
	if err != nil {
		return nil, fmt.Errorf("%s must hold a JSON array of strings: %w", name, err)
	}

	if len(items) > config.MaxAcrossItems {
		return nil, fmt.Errorf("%s holds %d items, above the limit of %d; filter the list where it is written, or split the run",
			name, len(items), config.MaxAcrossItems)
	}

	return items, nil
}
