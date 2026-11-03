package venue

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/shell"
)

// cachePairs is each cache's kept directory beside the step's place for it, shell-quoted, and the test that a symlink sits between, in a stable order.
func (s *bareSession) cachePairs() [][3]string {
	rels := slices.Sorted(maps.Keys(s.spec.Caches))
	pairs := make([][3]string, 0, len(rels))

	for _, rel := range rels {
		pairs = append(pairs, [3]string{s.root() + "/steps-task-caches/" + s.spec.Caches[rel], shellQuote(path.Join(s.dir, rel)), s.linkedAbove(rel)})
	}

	return pairs
}

// linkedAbove is a shell test, true when a directory between the step's directory and rel is a symlink: an uploaded input keeps its symlinks, and an rm or mv through one reaches outside the step.
func (s *bareSession) linkedAbove(rel string) string {
	var test strings.Builder

	test.WriteString("{ false")

	for dir := path.Dir(path.Clean(rel)); dir != "."; dir = path.Dir(dir) {
		test.WriteString(" || [ -L " + shellQuote(path.Join(s.dir, dir)) + " ]")
	}

	test.WriteString("; }")

	return test.String()
}

// attachCaches moves each kept cache over the uploaded stand-in, the rename the local workspace does, on the worker.
func (s *bareSession) attachCaches(ctx context.Context) error {
	if len(s.spec.Caches) == 0 {
		return nil
	}

	s.pipeline = shell.MetadataFrom(ctx).PipelineName

	var stdout, stderr strings.Builder

	_, err := s.exec(ctx, s.attachScript(), nil, &stdout, &stderr)
	if err != nil {
		return fmt.Errorf("restoring the step's caches: %w", err)
	}

	s.markAttached(stdout.String())

	// A cache is a speed-up, so one that cannot be put back is an empty cache, as on this machine, never a failed step.
	if len(s.attached) < len(s.spec.Caches) {
		slog.WarnContext(ctx, "venue.ssh.task_cache_restore_failed", "worker", s.worker.String(), "restored", len(s.attached), "caches", len(s.spec.Caches), "stderr", strings.TrimSpace(stderr.String()))
	}

	return nil
}

// markAttached reads attachScript's one "attached <i>" line per cache it put in place.
func (s *bareSession) markAttached(stdout string) {
	s.attached = map[int]bool{}

	for line := range strings.Lines(stdout) {
		i, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "attached "))
		if err == nil {
			s.attached[i] = true
		}
	}
}

func (s *bareSession) attachScript() string {
	var script strings.Builder

	for i, pair := range s.cachePairs() {
		// A failure leaves the cache where it was kept; the step gets an empty directory if one can be made.
		fmt.Fprintf(&script, `if ! %[3]s; then { rm -rf %[2]s && mkdir -p "$(dirname %[2]s)" && if [ -d %[1]s ]; then mv %[1]s %[2]s; else mkdir -p %[2]s; fi && echo attached %[4]d; } || mkdir -p %[2]s; fi; `, pair[0], pair[1], pair[2], i)
	}

	return script.String()
}

// detachCaches is the shell that files each cache back, replacing one a concurrent session filed first; run after the step's processes are gone.
func (s *bareSession) detachCaches() string {
	if len(s.attached) == 0 {
		return ""
	}

	var script strings.Builder

	script.WriteString("mkdir -p " + s.root() + "/steps-task-caches; ")

	// A concurrent close can file the entry between the rm and the mv, and a plain mv then moves this cache inside it. GNU mv -T refuses instead, and one retry keeps the last finisher's. ponytail: no -T outside Linux, so the race can still nest there; a lock is the upgrade.
	move := "mv %[2]s %[1]s"
	if s.goos == "linux" {
		move = "{ mv -T %[2]s %[1]s || { rm -rf %[1]s; mv -T %[2]s %[1]s; }; }"
	}

	for i, pair := range s.cachePairs() {
		if !s.attached[i] {
			continue
		}

		// The .pipeline file is what a later run of this pipeline judges the cache by when its config stops naming it.
		fmt.Fprintf(&script, `if ! %[4]s && [ -d %[2]s ]; then rm -rf %[1]s; `+move+` && printf %%s %[3]s > %[1]s.pipeline; fi; `, pair[0], pair[1], shellQuote(s.pipeline), pair[2])
	}

	return script.String()
}
