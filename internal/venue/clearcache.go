package venue

// Removing a step's caches: from the worker that keeps them, for steps clear-task-cache.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
)

var (
	// ErrKeepsNoCache is a launch-rung worker: its machine is terminated with the job, and its caches with it.
	ErrKeepsNoCache = errors.New("a machine launched for each job is terminated after it, and its caches with it")
	// ErrParked is a stopped instance, whose caches sit on a disk nothing can reach until it is started.
	ErrParked = errors.New("the worker is a stopped instance; its caches are on its disk")
)

// ClearedCache is one cache on a worker: whether it was there, or why it could not be removed.
type ClearedCache struct {
	Key   string
	Found bool
	Err   error
}

// ClearTaskCaches removes the caches keyed by keys (workspace.TaskCacheKey) from the worker raw names. A parked machine is started only when start says so, and parked again after; err is the worker never being reached, a cache that could not be removed is its own ClearedCache.Err.
func ClearTaskCaches(ctx context.Context, raw string, keys []string, start bool) (_ []ClearedCache, err error) {
	worker, err := ParseWorker(raw)
	if err != nil {
		return nil, err
	}

	switch worker.Rung {
	case RungLaunch:
		return nil, ErrKeepsNoCache
	case RungStopped:
		if !start {
			return nil, fmt.Errorf("%w (pass --start to start it, clear, and stop it again)", ErrParked)
		}

		static, release, acquireErr := acquire(ctx, worker)
		if acquireErr != nil {
			return nil, acquireErr
		}

		if release != nil {
			defer func() { err = errors.Join(err, release(ctx)) }()
		}

		worker = static
	case RungStatic:
	}

	if worker.dockerPlus() {
		return clearVolumes(ctx, worker, keys)
	}

	return clearDirs(ctx, worker, keys)
}

func clearVolumes(ctx context.Context, worker Worker, keys []string) ([]ClearedCache, error) {
	s := newPlusSession(worker, shell.RunnerSpec{})

	dial, err := s.daemonDialer(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, err)
	}

	defer func() {
		if client := s.conn.current(); client != nil {
			_ = client.Close()
		}
	}()

	docker, err := dockerapi.NewDialer(s.daemonName(), dial)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, err)
	}

	defer func() { _ = docker.Close() }()

	cleared := make([]ClearedCache, 0, len(keys))

	for _, key := range keys {
		name := taskCacheVolume(s.namespace, key)

		_, err = docker.InspectVolume(ctx, name)
		if dockerapi.IsNotFound(err) {
			cleared = append(cleared, ClearedCache{Key: key})

			continue
		}

		if err == nil {
			// A volume a container mounts is refused, never forced: the step using it would lose its files mid-run.
			err = docker.RemoveVolume(ctx, name)
		}

		cleared = append(cleared, ClearedCache{Key: key, Found: true, Err: err})
	}

	return cleared, nil
}

func clearDirs(ctx context.Context, worker Worker, keys []string) ([]ClearedCache, error) {
	client, err := sshClientFor(ctx, worker)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, err)
	}

	s := &bareSession{worker: worker}
	s.conn.adopt(client)

	defer func() { _ = client.Close() }()

	var script strings.Builder

	for _, key := range keys {
		// A key is a hex digest, but it reaches a shell, so it is quoted all the same.
		fmt.Fprintf(&script, `d=%[1]s/steps-task-caches/%[2]s; if [ ! -e "$d" ]; then echo %[2]s absent; elif rm -rf "$d"; then echo %[2]s removed; else echo %[2]s failed; fi; `, s.root(), shellQuote(key))
	}

	var stdout, stderr strings.Builder

	code, err := s.exec(ctx, script.String(), nil, &stdout, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("%w: exit %d: %s", errBareTransfer, code, strings.TrimSpace(stderr.String()))
	}

	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, err)
	}

	return readCleared(stdout.String(), stderr.String(), keys), nil
}

// readCleared reads clearDirs' one line per key; a key with no line was never reached, which is a failure rather than an absence.
func readCleared(stdout, stderr string, keys []string) []ClearedCache {
	outcome := map[string]string{}

	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		key, word, ok := strings.Cut(scanner.Text(), " ")
		if ok {
			outcome[key] = word
		}
	}

	cleared := make([]ClearedCache, 0, len(keys))

	for _, key := range keys {
		switch outcome[key] {
		case "absent":
			cleared = append(cleared, ClearedCache{Key: key})
		case "removed":
			cleared = append(cleared, ClearedCache{Key: key, Found: true})
		default:
			cleared = append(cleared, ClearedCache{Key: key, Found: true, Err: fmt.Errorf("%w: %s", errCacheNotRemoved, strings.TrimSpace(stderr))})
		}
	}

	return cleared
}

var errCacheNotRemoved = errors.New("the cache directory could not be removed")
