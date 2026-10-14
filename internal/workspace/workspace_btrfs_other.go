//go:build !linux

package workspace

import (
	"context"
	"fmt"
	"runtime"

	"github.com/jtarchie/steps/internal/config"
)

// newBtrfsProvider on non-Linux platforms always fails validation: btrfs
// subvolumes are a Linux kernel filesystem feature with no macOS/BSD
// equivalent. Config.validateWorkspace only checks schema shape at load
// time (it can't know the runtime GOOS), so this is where the platform
// check actually happens.
func newBtrfsProvider(*config.WorkspaceConfig, bool) (Provider, error) {
	return unsupportedBtrfsProvider{}, nil
}

type unsupportedBtrfsProvider struct{}

func (unsupportedBtrfsProvider) Validate() error {
	return fmt.Errorf("workspace strategy btrfs requires linux (running on %s): %w", runtime.GOOS, errUnsupportedPlatform)
}

func (unsupportedBtrfsProvider) NewBuild(context.Context, string) (BuildWorkspace, error) {
	return nil, fmt.Errorf("workspace strategy btrfs requires linux (running on %s): %w", runtime.GOOS, errUnsupportedPlatform)
}

func (unsupportedBtrfsProvider) Close() error { return nil }

// Probe answers as Validate does: there is no sweep to leave out.
func (p unsupportedBtrfsProvider) Probe() error { return p.Validate() }

// Reuse is unreachable: Validate refuses before any run starts.
func (unsupportedBtrfsProvider) Reuse(string) {}

func (unsupportedBtrfsProvider) AttachArtifactStore(BlobStore, StepIndex) bool { return false }
