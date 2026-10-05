package shell

// Which user a containerized command runs as, and why the default is not
// simply the image's.

import (
	"fmt"
	"os"
	"runtime"
)

// containerUser resolves the value handed to `docker run --user`: the
// pipeline's own user: when it set one, otherwise the platform default — and
// only when the daemon is THIS machine's.
//
// localDaemon is the whole subtlety. A docker+ worker's tree lives in volumes on
// its daemon, not in a bind mount from this machine, so this machine's uid:gid
// would be a --user computed on one machine for a tree on another: the image's
// own user is the answer there.
func containerUser(configured string, localDaemon bool) string {
	if configured != "" {
		return configured
	}

	if !localDaemon {
		return ""
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
//
// A variable so a test can pin a non-empty answer: on a machine where the
// platform default is already "" — any darwin orchestrator — a test that reads
// the ambient value cannot tell "deferred to the image" from "substituted this
// machine's uid", which is exactly the pair the caller has to keep apart.
//
//nolint:gochecknoglobals // a test seam over one platform answer, not state
var defaultContainerUser = func() string {
	return containerUserFor(runtime.GOOS, os.Getuid(), os.Getgid())
}

// containerUserFor is defaultContainerUser's rule over a machine's facts. A negative uid is "cannot say" (Windows), which defers to the image, as Concourse does with an unset user.
func containerUserFor(goos string, uid, gid int) string {
	if goos != "linux" || uid < 0 || gid < 0 {
		return ""
	}

	return fmt.Sprintf("%d:%d", uid, gid)
}
