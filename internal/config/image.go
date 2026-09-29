package config

// The image: field wherever it appears, and whether a pipeline needs a
// container runtime at all.

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// validateImageRules groups the three image:-related load-time checks
// (grouped into one call so config.validate's own branch count doesn't grow
// with every image: rule added — see cyclop): image: is invalid on get/put
// steps, an image: value must not look like a docker flag, and a fix: agent
// may not set its own image:.
func (c *Config) validateImageRules() error {
	err := c.validateImages()
	if err != nil {
		return err
	}

	err = c.validateImageValues()
	if err != nil {
		return err
	}

	err = c.validateArtifactImageEntries()
	if err != nil {
		return err
	}

	return c.validateFixAgentImages()
}

// validateImages rejects image: on get/put steps: a put's execution image
// comes from its resource type (ResourceType.Image), and a get step has no
// task/agent to scope.
func (c *Config) validateImages() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Image == "" {
				return nil
			}

			//kindswitch:ignore Task and Agent are the kinds image: is FOR — the cases here are the rejections
			switch {
			case step.Get != "":
				return fmt.Errorf("%s (get %q): image is not valid on get steps", label, step.Get)
			case step.Put != "":
				return fmt.Errorf("%s (put %q): image is not valid on put steps; set it on the resource_type instead", label, step.Put)
			case step.Try != nil:
				// A wrapper's own image: was accepted and then ignored:
				// resolveStepImage recurses into step.Try and reads the
				// wrapped step's image, never the wrapper's. Silently doing
				// nothing is worse than refusing.
				return fmt.Errorf("%s: image is not valid on a try: step; set it on the step try: wraps", label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateImageValues rejects an image: value that could be misread as a
// docker flag rather than an image reference: anything starting with '-'
// (e.g. "--privileged", "-v", "--network=host"). shell.dockerRunArgs also
// inserts a literal "--" before the image argument as defense in depth, but
// this check is what turns a mistyped or supply-chain-tainted image string
// into a clear LoadConfig error instead of docker silently granting whatever
// the flag means (privileged mode, an arbitrary bind mount, host
// networking). Checked wherever image: can be set: resource_types, agents,
// tasks, and steps (a step's own image: override).
func (c *Config) validateImageValues() error {
	for i := range c.ResourceTypes {
		rt := c.ResourceTypes[i]

		err := checkImageValue(fmt.Sprintf("resource_type %q", rt.Name), rt.Image)
		if err != nil {
			return err
		}
	}

	for i := range c.Agents {
		agent := c.Agents[i]

		err := checkImageValue(fmt.Sprintf("agent %q", agent.Name), agent.Image)
		if err != nil {
			return err
		}
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := checkImageValue(fmt.Sprintf("task %q", task.Name), task.Image)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return checkImageValue(label, step.Image)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkImageValue rejects an image value beginning with '-', which docker's
// argument parser would read as a flag rather than an image reference.
func checkImageValue(context, image string) error {
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("%s: image %q must not start with '-' (docker would parse it as a flag, not an image reference)", context, image)
	}

	return nil
}

// validateFixAgentImages rejects a fix: agent that sets its own image: —
// agent.RunFix always executes under the failing task's image (rt.Image),
// never the fix agent's own, so a fix agent's image: can never take effect.
// An unresolvable fix: agent name is left for FindAgent to catch at run
// time, same as everywhere else agent/task names aren't cross-checked at
// load time.
func (c *Config) validateFixAgentImages() error {
	check := func(context string, fix *FixSpec) error {
		if fix == nil {
			return nil
		}

		agent, err := c.FindAgent(fix.Agent)
		if err != nil {
			return nil //nolint:nilerr // unresolvable agent name is caught at run time, not here
		}

		if agent.Image != "" {
			return fmt.Errorf("%s: fix agent %q sets image: %q, but a fix loop always runs under the failing task's image, not the fix agent's own", context, fix.Agent, agent.Image)
		}

		return nil
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := check(fmt.Sprintf("task %q", task.Name), task.Fix)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return check(label, step.Fix)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// UsesImages reports whether any resource_type, agent, task, or step sets
// image: — used to fail fast (before any step runs) when docker isn't
// available but the pipeline needs it.
func (c *Config) UsesImages() bool {
	if len(c.Images()) > 0 {
		return true
	}

	// An artifact image is absent from Images() but still runs on this
	// daemon when its step is local.
	errLocalArtifact := errors.New("local artifact image")

	return c.visitContainerSettings(func(_ string, settings containerSettings) error {
		if settings.Artifact && len(settings.Tags) == 0 {
			return errLocalArtifact
		}

		return nil
	}) != nil
}

// Images returns every distinct image: this pipeline runs on THIS machine's
// daemon, sorted.
//
// Used to pull them all before the first step runs. Without that, the first
// command needing an uncached image pays the pull inside its own step: the
// progress output lands in whatever that command's stderr is being used for
// (a resource check's parsed output, an agent's tool result), and the download
// counts against the step's timeout, so a large image on a cold daemon can
// exhaust a budget meant for the work itself.
//
// A PLACED step's image is deliberately absent. Its container runs on the
// worker's daemon, which does not exist yet when this is asked — a machine
// acquired for the job has not been acquired — so pulling it here would
// download it to a machine that will never run it, and `docker image inspect`
// finding a LOCALLY built tag would skip the pull the worker actually needed.
// It is also what lets an orchestrator with no daemon at all run a pipeline
// whose every container lives on a worker, which is the arrangement the
// feature exists for.
func (c *Config) Images() []string {
	seen := map[string]bool{}

	placedOnly := c.placedOnlyEntries()

	_ = c.visitContainerSettings(func(_ string, settings containerSettings) error {
		// An artifact image is a name until its get runs; the step pulls
		// the resolved reference itself.
		if settings.Image == "" || len(settings.Tags) > 0 || settings.Artifact {
			return nil
		}

		// A tasks:/agents: entry is visited on its own and knows nothing
		// about who references it, so its image looks local even when every
		// step that uses it is placed. Named entries are kept only when some
		// step runs them HERE; a step's own image: is already covered by the
		// tags: check above.
		//
		// Keyed by KIND and name: a resource_type shares a label format with
		// the other two collections, which is how a resource_type named like
		// a placed task once lost its pre-pull.
		if placedOnly[settings.Entry] {
			return nil
		}

		seen[settings.Image] = true

		return nil
	})

	images := make([]string, 0, len(seen))
	for image := range seen {
		images = append(images, image)
	}

	slices.Sort(images)

	return images
}

// placedOnlyEntries names the tasks: and agents: entries that are referenced,
// and referenced ONLY by steps that run on a worker — the entries whose image
// this machine therefore never runs.
//
// Referenced-only, deliberately. An entry nothing mentions is left alone
// rather than pruned: a reference this scan does not know about would
// otherwise silently drop an image the pre-pull was supposed to fetch, and
// the step would fail on a machine that could have had it. Narrowing what is
// skipped is worth more here than pruning what is unused.
func (c *Config) placedOnlyEntries() map[entryRef]bool {
	referenced, local := map[entryRef]bool{}, map[entryRef]bool{}

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(_ string, step *Step) error {
			for _, entry := range []entryRef{{Kind: "task", Name: step.Task}, {Kind: "agent", Name: step.Agent}} {
				if entry.Name == "" {
					continue
				}

				referenced[entry] = true

				if len(step.Tags) == 0 {
					local[entry] = true
				}
			}

			return nil
		})
	}

	// A resource_type's commands run wherever its resources say. Every
	// resource of the type tagged means no check, in or out of it runs here
	// (a step's own tags: can only move one elsewhere); one untagged resource
	// keeps the image local, since the poller checks it from this machine.
	for _, resource := range c.Resources {
		entry := entryRef{Kind: "resource_type", Name: resource.Type}
		referenced[entry] = true

		if len(resource.Tags) == 0 {
			local[entry] = true
		}
	}

	placedOnly := map[entryRef]bool{}

	for entry := range referenced {
		if !local[entry] {
			placedOnly[entry] = true
		}
	}

	return placedOnly
}

// digestPattern is the only shape a version's digest: may take. It crosses a
// trust boundary — check output, a webhook payload, a --pin — and becomes a
// docker reference, so anything looser is refused rather than passed on.
var digestPattern = regexp.MustCompile(`^(sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)

// Fetched is what one get put in a build: its resource's source and the
// version it fetched — the two halves an artifact image is made of.
type Fetched struct {
	Source  map[string]any
	Version map[string]any
}

// ImageArtifacts names every value a step image: in jobName resolves as an
// artifact rather than as an image: each resource, and each get in the job
// (aliases included). A resource the job never fetches is included on
// purpose: otherwise its name would silently pull a Docker Hub image of that
// name. Artifact names cannot hold ':', '/' or '@', so only a bare image
// name can collide, and a resolved reference never does.
func (c *Config) ImageArtifacts(jobName string) map[string]bool {
	for _, job := range c.Jobs {
		if job.Name == jobName {
			return c.jobImageArtifacts(job)
		}
	}

	return c.jobImageArtifacts(Job{})
}

func (c *Config) jobImageArtifacts(job Job) map[string]bool {
	names := map[string]bool{}

	for _, resource := range c.Resources {
		names[resource.Name] = true
	}

	_ = job.visitSteps(func(_ string, step *Step) error {
		if step.Get != "" {
			names[step.Get] = true
		}

		return nil
	})

	return names
}

// ImageReference builds the image an artifact names: registry-image's
// convention, the source's repository at the version's digest.
//
// The repository comes ONLY from source. The version is whatever a check,
// webhook or pin said, and letting it name the repository would let any of
// those redirect a step to any image anywhere; the digest merely pins content
// inside the repository the pipeline's author chose. Errors never quote
// source, which routinely carries a registry password.
//
// ponytail: one convention for every resource type. A resource_type-level
// reference template is the upgrade path if a second shape appears.
func ImageReference(source, version map[string]any) (string, error) {
	repository, ok := source["repository"].(string)
	if !ok || repository == "" {
		return "", errors.New("the resource's source has no repository: string")
	}

	if strings.HasPrefix(repository, "-") || strings.Contains(repository, "@") ||
		strings.IndexFunc(repository, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("the resource's source repository: must not start with '-' or contain '@', whitespace or control characters")
	}

	raw, ok := version["digest"]
	if !ok {
		return "", errors.New("its version has no digest")
	}

	digest, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("its version's digest: is a %T, not a string", raw)
	}

	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("its version's digest %q is not sha256:<64 hex> or sha512:<128 hex>", digest)
	}

	return repository + "@" + digest, nil
}

// ResolveArtifactImage returns step with an image: naming an artifact
// replaced by the reference that artifact's get fetched, reporting whether
// anything was replaced. A try: is resolved through to the step it wraps,
// because that is the step that runs and the step its guard runs as.
//
// It returns a COPY and never writes through step: a try: body is cloned
// before it is resolved. A long-lived process hands one loaded Config to
// every run, and a digest written into it would be run N's image for run N+1
// — stale, and a false cache hit.
//
// The planner and the executor both call this, so they cannot disagree about
// the image a step hashes under.
func ResolveArtifactImage(step Step, artifacts map[string]bool, fetched map[string]Fetched) (Step, bool, error) {
	if step.Try != nil {
		inner, resolved, err := ResolveArtifactImage(*step.Try, artifacts, fetched)
		if err != nil || !resolved {
			return step, false, err
		}

		step.Try = &inner

		return step, true, nil
	}

	if !artifacts[step.Image] {
		return step, false, nil
	}

	got, ok := fetched[step.Image]
	if !ok {
		return step, false, fmt.Errorf("image %q names an artifact no get in this build fetched: get it first, or rename the resource if you meant the image %s", step.Image, step.Image)
	}

	reference, err := ImageReference(got.Source, got.Version)
	if err != nil {
		return step, false, fmt.Errorf("image %q: %w", step.Image, err)
	}

	step.Image = reference

	return step, true, nil
}

// validateArtifactImageEntries refuses an artifact image: where no get can
// reach it. A tasks:, agents: or resource_types: entry is shared and sees no
// job's gets (and a resource type's check runs before any job), and a
// job-level hook runs outside every triggered build.
//
// An entry may be named from any job, so a get alias in any job counts: a
// step naming it there would read the alias as an artifact, and the entry
// would silently pull a Docker Hub image of that name.
func (c *Config) validateArtifactImageEntries() error {
	artifacts := c.jobImageArtifacts(Job{})
	for _, job := range c.Jobs {
		maps.Copy(artifacts, c.jobImageArtifacts(job))
	}

	err := c.visitContainerSettings(func(context string, settings containerSettings) error {
		if settings.Entry.Kind == "" || !artifacts[settings.Image] {
			return nil
		}

		return fmt.Errorf("%s: image %q names a resource or get, but only a step's own image: can name an artifact; set image: on the step, or rename the resource or get if you meant the image %s",
			context, settings.Image, settings.Image)
	})
	if err != nil {
		return err
	}

	for _, job := range c.Jobs {
		artifacts := c.jobImageArtifacts(job)

		err := job.Hooks.Each(func(name string, hook *Step) error {
			return visitStepTree(fmt.Sprintf("%s %s hook", job.label(), name), hook, func(label string, step *Step) error {
				if artifacts[step.Image] {
					return fmt.Errorf("%s: image %q names an artifact, but a job-level hook runs outside any build and has no fetched gets; move it to a step's hook", label, step.Image)
				}

				return nil
			})
		})
		if err != nil {
			return err
		}
	}

	return nil
}
