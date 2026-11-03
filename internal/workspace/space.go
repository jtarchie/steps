package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

func (b *isolatingBuild) TaskSpace(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, error) {
	return b.newSpace(ctx, label, inputs, outputs, inputMapping, outputMapping)
}

func (b *isolatingBuild) GuardSpace(ctx context.Context, label string, inputs []string, inputMapping map[string]string, all bool) (StepSpace, error) {
	if all {
		names, err := b.allArtifacts()
		if err != nil {
			return nil, err
		}

		inputs, inputMapping = names, nil
	}

	present := make([]string, 0, len(inputs))

	for _, in := range inputs {
		// The MAPPED name is what the store holds; the declared name is only
		// what the directory is called inside the space. Checking the declared
		// one would read every mapped input as unproduced, which is the guard
		// answering false about an artifact that is right there.
		artifact := mappedName(in, inputMapping)

		// A name newSpace would reject is kept rather than skipped, so
		// leniency covers exactly "not produced" and the error still comes
		// from the one place that phrases it well.
		if config.ValidateArtifactName(in) != nil || config.ValidateArtifactName(artifact) != nil {
			present = append(present, in)

			continue
		}

		_, err := os.Lstat(filepath.Join(b.artifacts, artifact))
		if os.IsNotExist(err) {
			if _, held := b.remoteArtifact(artifact); !held {
				continue
			}
		}

		// Anything else — an unreadable directory, a symlinked artifact — is
		// newSpace's to report too.
		present = append(present, in)
	}

	return b.newSpace(ctx, label, present, nil, inputMapping, nil)
}

func (b *isolatingBuild) PutSpace(ctx context.Context, label string, inputs []string, all bool) (StepSpace, error) {
	if all {
		names, err := b.allArtifacts()
		if err != nil {
			return nil, err
		}

		inputs = names
	}

	return b.newSpace(ctx, label, inputs, nil, nil, nil)
}

// allArtifacts lists every artifact name currently in the build store, for a
// put step's inputs: all. Order is sorted so the materialized view is stable.
func (b *isolatingBuild) allArtifacts() ([]string, error) {
	entries, err := os.ReadDir(b.artifacts)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing has been produced yet
		}

		return nil, fmt.Errorf("could not list build artifacts: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && !strings.Contains(entry.Name(), stagedMarker) {
			names = append(names, entry.Name())
		}
	}

	for _, name := range b.remoteNames() {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	return names, nil
}

func (b *isolatingBuild) newSpace(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, error) {
	return b.newSpaceLeaving(ctx, label, inputs, outputs, inputMapping, outputMapping, nil)
}

// newSpaceLeaving is newSpace with some inputs left on the workers that hold
// them (PlacedTaskSpace): those are neither pulled nor materialized, and an
// output of the same name is not created empty over the tree the worker
// will place.
func (b *isolatingBuild) newSpaceLeaving(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string, leaving map[string]RemoteArtifact) (StepSpace, error) {
	// The build-global counter (not the plan index) numbers the directory, so
	// uniqueness never depends on the caller passing a distinct label.
	n := b.stepCounter.Add(1)
	dir := filepath.Join(b.stepsDir, fmt.Sprintf("%02d-%s", n, sanitizeLabel(label)))

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create step workspace %q: %w", dir, err)
	}

	err = b.materializeSpace(ctx, dir, inputs, outputs, inputMapping, outputMapping, leaving)
	if err != nil {
		// Tear down whatever was already materialized under dir, so a
		// mid-loop failure doesn't leak input copies until the
		// whole build is torn down (the caller never receives a StepSpace to
		// Close on this path).
		removeErr := b.backend.remove(dir)
		if removeErr != nil {
			slog.ErrorContext(ctx, "workspace.space_cleanup", "dir", dir, "error", removeErr)
		}

		return nil, err
	}

	return &isolatingSpace{
		backend: b.backend, artifacts: b.artifacts, dir: dir,
		outputs: outputs, outputMapping: outputMapping, keep: b.keep,
		captured: b.superseded, left: leaving,
	}, nil
}

// materializeSpace populates an already-created step directory with a copy or
// snapshot of each input under its declared name and an empty directory for
// each output — except an output that is also an input, which keeps the
// materialized content (read-modify-write; see validateArtifactNames). inputMapping/outputMapping rename a declared name to the plan-
// artifact name it draws from / captures to: the directory on disk keeps the
// declared name (what the task's run: expects), while the artifact copied in /
// captured out uses the mapped name. On any error the caller (newSpace)
// removes dir.
func (b *isolatingBuild) materializeSpace(ctx context.Context, dir string, inputs, outputs []string, inputMapping, outputMapping map[string]string, leaving map[string]RemoteArtifact) error {
	for _, in := range inputs {
		err := config.ValidateArtifactName(in)
		if err != nil {
			return fmt.Errorf("input %q: %w", in, err)
		}

		if _, left := leaving[in]; left {
			continue
		}

		artifact := mappedName(in, inputMapping)

		err = b.ensureLocal(ctx, artifact)
		if err != nil {
			return fmt.Errorf("input %q: %w", in, err)
		}

		src := filepath.Join(b.artifacts, artifact)

		err = rejectSymlinkSrc(src)
		if err != nil {
			return fmt.Errorf("input %q: %w", in, err)
		}

		err = b.backend.materialize(ctx, src, filepath.Join(dir, in))
		if err != nil {
			return fmt.Errorf("materializing input %q: %w", in, err)
		}
	}

	return b.materializeOutputs(ctx, dir, inputs, outputs, outputMapping)
}

// materializeOutputs is materializeSpace's output half, split out to stay
// within the linter's complexity budget.
func (b *isolatingBuild) materializeOutputs(ctx context.Context, dir string, inputs, outputs []string, outputMapping map[string]string) error {
	materialized := make(map[string]bool, len(inputs))
	for _, in := range inputs {
		materialized[in] = true
	}

	for _, out := range outputs {
		err := validateOutputName(out, outputMapping)
		if err != nil {
			return err
		}

		// An output that is also an input is read-modify-write: the input
		// materialization already populated the directory with the artifact's
		// current content, and Capture writes it back. Creating an empty
		// directory here would clobber exactly the state the step declared
		// it wants to carry forward.
		if materialized[out] {
			continue
		}

		err = b.backend.createEmpty(ctx, filepath.Join(dir, out))
		if err != nil {
			return fmt.Errorf("creating output %q: %w", out, err)
		}
	}

	return nil
}

// validateOutputName holds a declared output, and where outputMapping
// captures it, to the artifact naming rules.
//
// A mapped output can't smuggle a bad destination into the store on Capture.
// The PATH rule rather than the plain name rule, because a collecting matrix's
// cells legitimately capture under coordinates (findings/alpha/fast) —
// user-written mapping values are pinned to the strict name rule at load, so
// only machine-composed paths carry slashes here.
func validateOutputName(out string, outputMapping map[string]string) error {
	err := config.ValidateArtifactName(out)
	if err != nil {
		return fmt.Errorf("output %q: %w", out, err)
	}

	if mapped := mappedName(out, outputMapping); mapped != out {
		err = config.ValidateArtifactPath(mapped)
		if err != nil {
			return fmt.Errorf("output %q (mapped to %q): %w", out, mapped, err)
		}
	}

	return nil
}

// isolatingSpace is one step's materialized directory under an isolating
// provider: an <input>/ copy or snapshot of each declared input, plus an
// empty <output>/ directory for each declared output.
type isolatingSpace struct {
	backend       treeBackend
	artifacts     string
	dir           string
	outputs       []string
	outputMapping map[string]string
	// keep mirrors the build's --keep-workspace: the step directory is left
	// on disk at Close, so the files a failed step had just written — the
	// first thing anyone wants to look at — survive to be read.
	keep bool
	// captured tells the build which artifacts this step just replaced, so a
	// digest remembered for one of them is not served to the next step's cache
	// key after the bytes behind it changed.
	captured func(names []string)
	// left are the inputs this space left on their holders (PlacedTaskSpace). One
	// that is also an output has no directory here when the worker kept the
	// result: the record of the hold IS the capture.
	left map[string]RemoteArtifact
}

func (s *isolatingSpace) Dir() string { return s.dir }

// Kept implements the optional capability workspace.Kept reads.
func (s *isolatingSpace) Kept() bool { return s.keep }

// Capture persists each declared output back into the build's artifact
// store, replacing whatever is already at its DESTINATION — the plain
// artifact name for an ordinary step, a per-cell coordinate subdirectory
// (findings/alpha) for a cell of a collecting matrix. Ordinary steps run
// sequentially, so a plain-name capture has no concurrent writer to race;
// collecting cells under max_in_flight: capture concurrently, and are safe
// only because CollectedOutputMapping gives every cell a disjoint
// destination and the block reset (ResetArtifact) has already created
// the shared parent. A collecting cell therefore replaces only its own
// subdirectory — whole-artifact replacement for the block is the reset's
// job, before any cell runs. A
// declared output directory that no longer exists when the step finished is
// an error: the step promised to produce it. rejectSymlinkSrc additionally
// refuses a declared output that is itself a symlink: the step's own run:/
// tool commands could otherwise delete the materialized output directory
// and replace it with a symlink to an arbitrary host path (e.g. /etc, a
// home directory), which the copy backend's `cp ... src/. dst` would then
// silently dereference, exfiltrating the real target's contents into the
// pipeline's artifact store as if it were the legitimate output.
func (s *isolatingSpace) Capture(ctx context.Context) error {
	if s.captured != nil {
		// Before the loop, not after: a capture that fails partway has still
		// replaced the artifacts it got to, and those digests are stale
		// whether or not the step as a whole succeeded.
		defer s.captured(artifactNames(s.outputs, s.outputMapping))
	}

	for _, out := range s.outputs {
		src := filepath.Join(s.dir, out)

		if _, left := s.left[out]; left {
			_, statErr := os.Lstat(src)
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
		}

		err := rejectSymlinkSrc(src)
		if err != nil {
			return fmt.Errorf("step declared output %q: %w", out, err)
		}

		// The directory on disk carries the declared name; output_mapping
		// captures it back into the store under the plan-artifact name.
		artifact := mappedName(out, s.outputMapping)
		dst := filepath.Join(s.artifacts, artifact)

		// Staged, so an interrupted capture leaves the previous artifact for a
		// --resume rather than an empty one.
		err = replaceTree(dst, s.backend.remove, func(staged string) error {
			return s.backend.materialize(ctx, src, staged)
		})
		if err != nil {
			return fmt.Errorf("capturing output %q: %w", out, err)
		}
	}

	return nil
}

// Close removes the step's directory. s.dir is a plain directory (created
// directly by newSpace) wrapping the backend's input/output copies, so
// it goes through backend.remove — see isolatingBuild.Close. Under
// --keep-workspace the directory stays, like everything else in the tree.
func (s *isolatingSpace) Close() error {
	if s.keep {
		return nil
	}

	err := s.backend.remove(s.dir)
	if err != nil {
		return fmt.Errorf("could not remove step workspace %q: %w", s.dir, err)
	}

	return nil
}
