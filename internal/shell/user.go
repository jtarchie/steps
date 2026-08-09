package shell

// Which user a containerized command runs as, and why the default is not
// simply the image's.

import (
	"fmt"
	"os"
	"runtime"
)

// containerUser resolves the value handed to `docker run --user`: the
// pipeline's own user: when it set one, otherwise the platform default.
func containerUser(configured string) string {
	if configured != "" {
		return configured
	}

	return defaultContainerUser()
}

// defaultContainerUser returns the user a containerized command runs as when
// the pipeline says nothing.
//
// On Linux it is the uid:gid that started steps. This is not a hardening
// measure — it fixes a correctness bug. A step's working directory is
// bind-mounted from the host with no uid translation, so a container running
// as root (which most images do) writes root-owned files into it. Three things
// then break, all of them confusing:
//
//   - An agent's file tools run host-side while its run_shell runs in the
//     container. So the agent creates a file with one of its own tools and
//     then cannot edit it with another, mid-conversation, for no reason it can
//     see or fix.
//   - Workspace capture and cleanup (os.RemoveAll, the copy backend) hit
//     EACCES on files the step legitimately produced.
//   - Anything the step leaves behind needs root to delete, long after the run.
//
// Elsewhere the mismatch does not arise: Docker Desktop's VM maps ownership on
// bind mounts, so the host user already owns what a container writes, and
// forcing a uid there would instead break images whose own files (a prebuilt
// virtualenv, a package cache) are owned by the user the image expects. So the
// default is deliberately platform-specific rather than uniform.
//
// The cost is real and is the reason user: exists: an image that installs
// packages at run time (apt-get, apk add) or writes to a root-owned path needs
// root, and under this default it fails. That failure is loud and local to the
// step, which is the trade being made against a silent, remote one.
func defaultContainerUser() string {
	if runtime.GOOS != "linux" {
		return ""
	}

	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}
