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
func newBtrfsProvider(*config.WorkspaceConfig) Provider {
	return unsupportedBtrfsProvider{}
}

type unsupportedBtrfsProvider struct{}

func (unsupportedBtrfsProvider) Validate() error {
	return fmt.Errorf("workspace strategy btrfs requires linux (running on %s): %w", runtime.GOOS, errUnsupportedPlatform)
}

func (unsupportedBtrfsProvider) NewBuild(context.Context, string) (BuildWorkspace, error) {
	return nil, fmt.Errorf("workspace strategy btrfs requires linux (running on %s): %w", runtime.GOOS, errUnsupportedPlatform)
}

func (unsupportedBtrfsProvider) Close() error { return nil }
