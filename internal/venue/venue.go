package venue

// The decision one tier above shell's.

import (
	"context"
	"os"
	"sync"

	"github.com/jtarchie/steps/internal/blobstore"
	"github.com/jtarchie/steps/internal/shell"
)

// NewRunner returns a runner for spec, on a worker when spec.Worker names one
// and on this machine otherwise.
//
// The empty case hands straight back to shell.NewRunner, unchanged and
// undecorated. That is deliberate: placement is opt-in, and a pipeline that
// never mentions a worker must take exactly the path it took before this
// package existed — same runner, same behavior, nothing wrapped.
func NewRunner(spec shell.RunnerSpec) (shell.Runner, error) {
	if spec.Worker == "" {
		return shell.NewRunner(spec) //nolint:wrapcheck // the local path is shell's answer, returned as shell phrased it
	}

	worker, err := ParseWorker(spec.Worker)
	if err != nil {
		return nil, err
	}

	blobs, err := artifactStoreFor(spec.ArtifactStore)
	if err != nil {
		return nil, err
	}

	worker.ArtifactStore = spec.ArtifactStore

	return runner{session: &session{
		worker:   worker,
		cwd:      spec.Cwd,
		outputs:  spec.Fetch,
		fetchAll: spec.FetchAll,
		env:      withWorkerTag(resolveEnv(spec.Env), spec.WorkerTag),
		tag:      spec.WorkerTag,
		keep:     spec.Keep,
		blobs:    blobs,
		// The container half of a placed step, if it has one. Kept as the
		// caller's own spec so nothing about what a container means is
		// re-decided here: the tree still travels the venue's way, and the
		// command runs through the same code every local containerized step
		// uses, pointed at the worker's daemon.
		container: spec,
	}}, nil
}

// artifactStores caches one client per store URL. Without it every placed
// step — and every guard, and every SSM bootstrap — rebuilt the client and
// re-resolved credentials from scratch: on EC2 that is an IMDS round trip per
// step, and on SSO a token dance, for the same answer every time.
//
//nolint:gochecknoglobals // a cache over a config-derived singleton, not state
var artifactStores sync.Map

// artifactStoreFor opens the blob store a spec names, or nothing — trees then
// ride the tunnel, which is always the floor.
func artifactStoreFor(raw string) (*blobstore.Store, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // absence is the documented answer: no store, use the tunnel
	}

	if cached, ok := artifactStores.Load(raw); ok {
		return cached.(*blobstore.Store), nil //nolint:forcetypeassert // this map holds one type
	}

	opts, err := blobstore.Parse(raw)
	if err != nil {
		return nil, err //nolint:wrapcheck // blobstore's errors name the URL and the rule it broke
	}

	// Opening reads only local configuration, so the constructor's missing
	// context is not hiding a network round trip.
	store, err := blobstore.New(context.Background(), opts)
	if err != nil {
		return nil, err //nolint:wrapcheck // as above
	}

	cached, _ := artifactStores.LoadOrStore(raw, store)

	return cached.(*blobstore.Store), nil //nolint:forcetypeassert // as above
}

// resolveEnv reads the values of the variables a pipeline's env: opted into.
//
// Resolved here, on the orchestrator, because that is where the operator's
// environment is. A name that is not set contributes nothing rather than an
// empty value — the same rule the host path follows, and for the same reason:
// the two are different to a command that tests for presence, and inventing an
// empty one turns a forgotten export into a silent misconfiguration.
func resolveEnv(names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}

	values := make(map[string]string, len(names))

	for _, name := range names {
		value, ok := os.LookupEnv(name)
		if ok {
			values[name] = value
		}
	}

	return values
}

// WorkerEnv is the variable a placed command can read to learn where it is.
//
// It exists because placement was otherwise entirely invisible from inside a
// step: a script that needs to behave differently on a worker — or a person
// reading a log wondering whether the tag took effect — had nothing to ask.
// Absent, rather than empty, for a step running on this machine, so the two
// are distinguishable to a command that tests for presence.
const WorkerEnv = "STEPS_WORKER"

func withWorkerTag(env map[string]string, tag string) map[string]string {
	if tag == "" {
		return env
	}

	if env == nil {
		env = map[string]string{}
	}

	env[WorkerEnv] = tag

	return env
}
