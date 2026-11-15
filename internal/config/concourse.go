package config

// A resource type that is one of Concourse's own resource images (steps#237), spelled as Concourse spells it: `type: registry-image` with `source: {repository, tag}`. It is rewritten at load into what steps already runs — an image: and three fixed scripts — and marked, so internal/resource hands those scripts Concourse's request on stdin rather than rendering it into a command line.

import (
	"errors"
	"fmt"
	"maps"
	"strings"
)

// RegistryImageType is the one type: a resource_types: entry may name, as in Concourse.
const RegistryImageType = "registry-image"

// The scripts a Concourse type runs. in and out are handed the directory as $1, as Concourse's executables expect, and Concourse's build variables, spelled from steps' own: a resource that reports where it was run (a notifier, a git commit message) reads these names.
const (
	concourseCheck = "exec /opt/resource/check"
	concourseBuild = `BUILD_ID="${STEPS_RUN_ID-}" BUILD_NAME="${STEPS_RUN_ID-}" BUILD_JOB_NAME="${STEPS_JOB_NAME-}" BUILD_PIPELINE_NAME="${STEPS_PIPELINE_NAME-}" BUILD_TEAM_NAME=main ATC_EXTERNAL_URL="${STEPS_URL-}" `
	concourseIn    = concourseBuild + `exec /opt/resource/in "$PWD"`
	concourseOut   = concourseBuild + `exec /opt/resource/out "$PWD"`
)

var errNotRegistryImage = errors.New("only type: registry-image is supported, naming a Concourse resource image in source.repository")

// desugarConcourseTypes rewrites every type: registry-image entry into an image: and the three scripts, and lays its defaults: under each of its resources' source:. Before validate(), so every rule about images, puts and gets judges the rewritten type exactly as it would a hand-written one.
func (c *Config) desugarConcourseTypes() error {
	var errs []error

	for i := range c.ResourceTypes {
		rt := &c.ResourceTypes[i]
		if rt.Type == "" && rt.Source == nil && rt.Defaults == nil {
			continue
		}

		err := desugarConcourse(rt)
		if err != nil {
			errs = append(errs, fmt.Errorf("resource_type %q: %w", rt.Name, err))

			continue
		}

		for j := range c.Resources {
			if c.Resources[j].Type == rt.Name {
				c.Resources[j].Source = withDefaults(rt.Defaults, c.Resources[j].Source)
			}
		}
	}

	return errors.Join(errs...)
}

func desugarConcourse(rt *ResourceType) error {
	err := refuseBesideRegistryImage(*rt)
	if err != nil {
		return err
	}

	repository, _ := rt.Source["repository"].(string)
	if strings.TrimSpace(repository) == "" {
		return errors.New("type: registry-image needs source.repository, the image to run, e.g. concourse/git-resource")
	}

	tag := "latest"
	if given, ok := rt.Source["tag"]; ok {
		tag = ""
		if given != nil {
			// YAML reads an unquoted 1.10 as the number 1.1, so stringifying it would silently pull another image.
			s, ok := given.(string)
			if !ok {
				return fmt.Errorf("source.tag must be a string, and YAML read this one as %v; quote it as written, e.g. tag: \"1.10\"", given)
			}

			tag = strings.TrimSpace(s)
		}

		if tag == "" {
			return errors.New("source.tag is empty; omit it for latest, or name the tag")
		}
	}

	rt.Image = repository + ":" + tag
	rt.Config = ResourceTypeConfig{Check: concourseCheck, In: concourseIn, Out: concourseOut, Concourse: true}

	return nil
}

// refuseBesideRegistryImage is everything a type: registry-image entry may not also say.
func refuseBesideRegistryImage(rt ResourceType) error {
	if rt.Type != RegistryImageType {
		return errNotRegistryImage
	}

	if rt.Image != "" {
		return errors.New("type: and image: both name the image; keep type: registry-image with source.repository")
	}

	return refuseConfigBesideRegistryImage(rt)
}

func refuseConfigBesideRegistryImage(rt ResourceType) error {
	if rt.Config.Check != "" || rt.Config.In != "" || rt.Config.Out != "" || rt.Config.MCP != nil || rt.Config.Expr != nil {
		return errors.New("type: registry-image runs the image's own /opt/resource scripts, so it takes no config")
	}

	for key := range rt.Source {
		if key != "repository" && key != "tag" {
			return fmt.Errorf("source.%s is not supported on a resource type; only repository and tag are", key)
		}
	}

	return nil
}

// withDefaults is Concourse's defaults: — a type's source keys under the resource's own, which win.
func withDefaults(defaults, source map[string]any) map[string]any {
	if len(defaults) == 0 {
		return source
	}

	merged := maps.Clone(defaults)
	maps.Copy(merged, source)

	return merged
}
