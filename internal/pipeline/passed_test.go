package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// A job hook's put is published after the build went green, and it passes the job together with what that build fetched: recorded apart, a downstream fan-in over both never opens.
func TestAJobHooksPutPassesWithTheBuildItFollowed(t *testing.T) {
	f := newPutFixture(t, strings.Replace(putTypes, "OUT", `echo '{"n":"shipped"}'`, 1)+`
- name: release
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: image
  on_success:
    put: release
`)

	err := f.run(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	together, err := VersionSetPassedUpstream(t.Context(), f.st, "build", map[string]map[string]any{
		"image":   {"n": "old"},
		"release": {"n": "shipped"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !together {
		t.Error("the hook's put did not pass build together with the get it followed")
	}
}

// The cap is the largest version recorded, not the first size refused.
func TestAVersionAtTheSizeCapIsRecorded(t *testing.T) {
	overhead := len(`{"n":""}`)

	_, ok := versionRecordable(t.Context(), "image", map[string]any{"n": strings.Repeat("x", maxRecordedVersionBytes-overhead)})
	if !ok {
		t.Errorf("a version of exactly %d bytes was refused", maxRecordedVersionBytes)
	}

	captureStdout(t, func() {
		_, ok = versionRecordable(t.Context(), "image", map[string]any{"n": strings.Repeat("x", maxRecordedVersionBytes-overhead+1)})
	})
	if ok {
		t.Errorf("a version of %d bytes was recorded", maxRecordedVersionBytes+1)
	}
}

// refusingVersions fails every write the build-end bookkeeping makes.
type refusingVersions struct{ store.Store }

func (refusingVersions) RecordVersionOrder(context.Context, string, string) (int64, error) {
	return 0, os.ErrPermission
}

func (refusingVersions) RecordCheckedVersion(context.Context, string, string) error {
	return os.ErrPermission
}

func (refusingVersions) RecordPassedVersion(context.Context, string, string, string, string) error {
	return os.ErrPermission
}

// The bookkeeping is best-effort and never fails the build, so a lost write's warning is the only trace that a passed: gate downstream will stay shut.
func TestALostVersionWriteIsWarned(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, versions := withBuildVersions(t.Context())
	version := map[string]any{"ref": "abc"}

	recordPutOrder(ctx, refusingVersions{}, "image", version)
	recordResolvedVersion(ctx, refusingVersions{}, &config.Config{}, "image", version, false)
	recordBuildVersion(ctx, "image", version)
	recordPassedVersions(ctx, refusingVersions{}, "build", "run-1", versions)

	for _, want := range []string{"job.put_order_unrecorded", "job.resolved_version_unrecorded", "job.passed_unrecorded"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("no %s warning; log:\n%s", want, buf.String())
		}
	}
}
