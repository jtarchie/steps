package resource

// The expr: backend's three stages. The language and its builtins live in
// internal/exprlang; what lives here is everything that touches the outside
// world on its behalf — writing an in:'s files to disk, and deciding which
// directory an out: may read.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/exprlang"
)

// exprInput assembles what an expression evaluates against. The env allowlist
// comes from the resource type's own env:, which is the whole set env() will
// resolve — see exprlang's envFunc for why there is no baseline beneath it.
func exprInput(rt config.ResourceType, source, version, params map[string]any, dir string) exprlang.Input {
	return exprlang.Input{
		Source:   source,
		Version:  version,
		Params:   params,
		EnvAllow: rt.Env,
		Dir:      dir,
	}
}

// exprCheckVersions evaluates expr.check and returns its versions.
func exprCheckVersions(
	ctx context.Context, rt config.ResourceType, source, version map[string]any,
) ([]map[string]any, error) {
	if rt.Config.Expr.Check == "" {
		return nil, fmt.Errorf("check %q: this resource type sets no expr.check, so it can only be published to", rt.Name)
	}

	slog.Debug("resource.check", "resource_type", rt.Name, "source", source, "version", version, "backend", "expr")

	versions, err := exprlang.RunCheck(ctx, rt.Config.Expr.Check, exprInput(rt, source, version, nil, ""))
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	slog.Info("resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

// exprRunIn evaluates expr.in and writes the files it produced into destDir.
//
// With no expr.in the version object is written as version.json and nothing
// else happens — the same default the mcp backend takes, and the common case
// for a type whose whole job is detecting that something changed.
func exprRunIn(
	ctx context.Context, rt config.ResourceType, source, version, params map[string]any, destDir string,
) error {
	if rt.Config.Expr.In == "" {
		return writeJSONFile(filepath.Join(destDir, "version.json"), version)
	}

	slog.Debug("resource.in", "resource_type", rt.Name, "source", source, "version", version,
		"params", params, "dest_dir", destDir, "backend", "expr")

	files, err := exprlang.RunIn(ctx, rt.Config.Expr.In, exprInput(rt, source, version, params, ""))
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	err = writeArtifactFiles(destDir, files)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	slog.Info("resource.fetched", "resource_type", rt.Name, "dest_dir", destDir, "files", len(files))

	return nil
}

// writeArtifactFiles writes an in:'s file map into the artifact directory.
//
// Every path is confined to destDir. filepath.IsLocal is the whole guard: it
// rejects absolute paths, "..", and the roundabout spellings of ".." that a
// prefix check misses. An expression is data from a pipeline file, and a
// pipeline file is not a licence to write anywhere on the machine.
func writeArtifactFiles(destDir string, files map[string]string) error {
	for path, contents := range files {
		if !filepath.IsLocal(path) {
			return fmt.Errorf("file %q: must be a relative path inside the artifact directory", path)
		}

		full := filepath.Join(destDir, path)

		err := os.MkdirAll(filepath.Dir(full), 0o750)
		if err != nil {
			return fmt.Errorf("file %q: %w", path, err)
		}

		err = os.WriteFile(full, []byte(contents), 0o600)
		if err != nil {
			return fmt.Errorf("file %q: %w", path, err)
		}
	}

	return nil
}

// exprRunOut evaluates expr.out, with file() scoped to srcDir — the put's
// read view, the same directory a shell out: gets as its cwd.
func exprRunOut(
	ctx context.Context, rt config.ResourceType, source, params map[string]any, srcDir string,
) (map[string]any, error) {
	if rt.Config.Expr.Out == "" {
		return nil, fmt.Errorf("out %q: this resource type sets no expr.out", rt.Name)
	}

	slog.Debug("resource.out", "resource_type", rt.Name, "source", source, "params", params,
		"src_dir", srcDir, "backend", "expr")

	version, err := exprlang.RunOut(ctx, rt.Config.Expr.Out, exprInput(rt, source, nil, params, srcDir))
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	slog.Info("resource.put", "resource_type", rt.Name, "src_dir", srcDir, "result", version)

	return version, nil
}

// CompileExprPrograms type-checks every expression in the pipeline without
// running any of them.
//
// It exists because internal/config cannot: that package imports nothing
// internal and no third-party code but the YAML parser, which is what keeps
// the config types a leaf every other package can agree on. So a syntax error
// in an expression is not a load error. This is what makes it a `steps
// validate` error instead, and preflightResource makes it a preflight one —
// both before anything polls.
func CompileExprPrograms(cfg *config.Config) error {
	for _, rt := range cfg.ResourceTypes {
		if rt.Config.Backend() != config.BackendExpr {
			continue
		}

		for _, slot := range exprSlots(rt) {
			err := exprlang.Compile(slot.name, slot.src)
			if err != nil {
				return fmt.Errorf("resource_type %q: %w", rt.Name, err)
			}
		}
	}

	return nil
}

// exprSlots lists the expressions a type actually declares, so an omitted
// stage is not compiled as an empty program.
func exprSlots(rt config.ResourceType) []struct {
	name exprlang.Slot
	src  string
} {
	var slots []struct {
		name exprlang.Slot
		src  string
	}

	for _, slot := range []struct {
		name exprlang.Slot
		src  string
	}{
		{exprlang.SlotCheck, rt.Config.Expr.Check},
		{exprlang.SlotIn, rt.Config.Expr.In},
		{exprlang.SlotOut, rt.Config.Expr.Out},
	} {
		if slot.src != "" {
			slots = append(slots, slot)
		}
	}

	return slots
}
