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
// localDaemon is the whole subtlety. A venue forwards a socket to a worker and
// has already made this decision there, from the worker's platform and the
// identity its shim runs as; an empty answer out of that is a real answer,
// meaning "defer to the image" — for a darwin worker, or one whose shim cannot
// vouch for a uid. Read here as "the pipeline said nothing" it was replaced
// with the ORCHESTRATOR's uid:gid: a --user computed on one machine for a bind
// mount on another, which is precisely the wrong-machine answer
// DefaultContainerUserFor exists to prevent. A Linux orchestrator against a
// root shim yields --user 1000:1000 over a root-owned 0700 workdir, and the
// step cannot write the outputs it declares.
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
	return DefaultContainerUserFor(runtime.GOOS, os.Getuid(), os.Getgid())
}

// DefaultContainerUserFor is defaultContainerUser's rule, applied to a
// machine's facts rather than this one's.
//
// Exported because a PLACED containerized step bind-mounts a tree on the
// WORKER, so every failure the rule prevents happens there, against the
// identity the shim runs as — and the platform question is the worker's too.
// Computing it from this process would answer about the wrong machine
// entirely: a Linux orchestrator against a root shim yields --user 1000:1000
// over a root-owned 0700 workdir, which cannot even be read.
//
// A negative uid is "cannot say" — Windows has no answer, and neither does a
// shim too old to send one. That defers to the image, which is what Concourse
// does with an unset user; it is the right answer wherever the daemon is not
// bind-mounting a foreign-owned tree, and the only honest one when the
// identity is unknown.
func DefaultContainerUserFor(goos string, uid, gid int) string {
	if goos != "linux" || uid < 0 || gid < 0 {
		return ""
	}

	return fmt.Sprintf("%d:%d", uid, gid)
}
