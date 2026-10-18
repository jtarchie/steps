package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jtarchie/steps/internal/pipeline"
)

// ClearTaskCacheCmd is fly clear-task-cache: it empties a step's caches: so its next run starts cold.
type ClearTaskCacheCmd struct {
	VarFlags  `embed:""`
	Pipeline  string            `arg:""                                                                                   help:"path to the pipeline YAML file"`
	Job       string            `help:"the job the step is in"                                                            required:""                           short:"j"`
	Step      string            `help:"the task: or agent: name the step runs"                                            required:""                           short:"s"`
	CachePath string            `help:"clear only this cache path (default: every one the step declares)"                 name:"cache-path"                     short:"c"`
	Worker    map[string]string `help:"map a step tag to a worker, as steps run takes it; a placed step's cache is there" name:"worker"`
	Start     bool              `help:"start a stopped aws:// or gcp:// worker to clear it, and stop it again"            name:"start"`
	// Name alone of StateFlags: the cache is keyed by the pipeline's name, and nothing here opens the state database.
	Name map[string]string `help:"name the pipeline as steps run --name does, e.g. --name infra=infra/pipeline.yml" name:"name"`
}

var errCachesNotCleared = errors.New("some caches were not cleared")

// Run clears what it can and fails if anything it found could not be removed.
func (c *ClearTaskCacheCmd) Run() error {
	cfg, err := c.Load(c.Pipeline, resolvePipelineName(c.Pipeline, c.Name))
	if err != nil {
		return err
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	ctx, err = pipeline.WithWorkers(ctx, c.Worker)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	cleared, err := pipeline.ClearTaskCaches(ctx, cfg, c.Job, c.Step, c.CachePath, c.Start)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if !printCleared(cleared) {
		return errCachesNotCleared
	}

	return nil
}

// printCleared reports each cache, failures on stderr, and whether every one was cleared.
func printCleared(cleared []pipeline.ClearedCache) bool {
	ok := true

	for _, one := range cleared {
		switch {
		case one.Err != nil && one.Path == "":
			ok = false

			fmt.Fprintf(os.Stderr, "%s: %v\n", one.Where, one.Err)
		case one.Err != nil:
			ok = false

			fmt.Fprintf(os.Stderr, "could not clear %s on %s: %v\n", one.Path, one.Where, one.Err)
		case one.Note != "":
			fmt.Printf("%s: %s\n", one.Where, one.Note)
		case one.Found:
			fmt.Printf("cleared %s on %s\n", one.Path, one.Where)
		default:
			fmt.Printf("%s on %s: nothing kept\n", one.Path, one.Where)
		}
	}

	return ok
}
