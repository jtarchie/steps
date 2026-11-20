package pipeline

import (
	"context"
	"fmt"
	"maps"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/workspace"
)

// Upload is a folder on this machine that `steps run --target --input` sent in place of a get.
type Upload struct {
	Dir string
	// Digest names the folder's bytes, so the step cache answers a second upload of the same tree.
	Digest string
}

func (u Upload) version() map[string]any { return map[string]any{"upload": u.Digest} }

type uploadsKey struct{}

// uploads is a run's Run.Inputs, with each resource an upload stands in for.
type uploads struct {
	byGet     map[string]Upload
	resources map[string]map[string]any
}

// WithUploads is how a resume judged before RunJob starts knows the versions uploads made, which no resource history holds.
func WithUploads(ctx context.Context, job *config.Job, inputs map[string]Upload) context.Context {
	if len(inputs) == 0 {
		return ctx
	}

	gets := job.Gets()
	held := uploads{byGet: maps.Clone(inputs), resources: map[string]map[string]any{}}

	for get, upload := range inputs {
		if resource, ok := gets[get]; ok {
			held.resources[resource] = upload.version()
		}
	}

	return context.WithValue(ctx, uploadsKey{}, held)
}

func uploadsFrom(ctx context.Context) uploads {
	held, _ := ctx.Value(uploadsKey{}).(uploads)

	return held
}

// uploadedVersions is what the run's resource.Cache answers each uploaded get with.
func uploadedVersions(ctx context.Context) map[string]map[string]any {
	held := uploadsFrom(ctx)
	if held.byGet == nil {
		return nil
	}

	versions := make(map[string]map[string]any, len(held.byGet))
	for get, upload := range held.byGet {
		versions[get] = upload.version()
	}

	return versions
}

// uploadedResource reports whether a get of resource was replaced by an upload, whose check is never asked.
func uploadedResource(ctx context.Context, resource string) bool {
	return uploadsFrom(ctx).resources[resource] != nil
}

// isUploadedVersion is a version an upload made: a one-off version of its get that the resource's history, its passed: record and its latest never see.
func isUploadedVersion(ctx context.Context, resource string, version map[string]any) bool {
	uploaded := uploadsFrom(ctx).resources[resource]

	return uploaded != nil && maps.Equal(uploaded, version)
}

// fetchUpload fills the get's artifact from its upload in place of the resource's in:, reporting false for a get nobody uploaded.
func fetchUpload(ctx context.Context, artifact string, bw workspace.BuildWorkspace) (bool, error) {
	upload, ok := uploadsFrom(ctx).byGet[artifact]
	if !ok {
		return false, nil
	}

	notef(ctx, "get: %s (uploaded: %s)", artifact, upload.Digest)

	_, err := bw.FetchResource(ctx, artifact, "", func(dir string) error {
		return workspace.CopyTree(ctx, upload.Dir, dir)
	})
	if err != nil {
		return true, fmt.Errorf("could not place the upload for %q: %w", artifact, err)
	}

	return true, nil
}
