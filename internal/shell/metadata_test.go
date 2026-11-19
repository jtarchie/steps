package shell

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestBuildMetadataReachesAHostCommand(t *testing.T) {
	t.Parallel()

	ctx := WithBuildMetadata(context.Background(), BuildMetadata{
		RunID: "RUN1", JobName: "job", PipelineName: "pipe", PipelineRevision: "abc", URL: "http://x",
	})

	out, err := RunCapture(ctx, HostRunner{cwd: t.TempDir()}, `echo "$STEPS_RUN_ID $STEPS_JOB_NAME $STEPS_PIPELINE_NAME $STEPS_PIPELINE_REVISION $STEPS_URL"`)
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}

	if got := strings.TrimSpace(string(out)); got != "RUN1 job pipe abc http://x" {
		t.Errorf("got %q", got)
	}
}

func TestBuildMetadataUnsetFieldsAreAbsentNotEmpty(t *testing.T) {
	t.Parallel()

	ctx := WithBuildMetadata(context.Background(), BuildMetadata{RunID: "RUN1"})

	out, err := RunCapture(ctx, HostRunner{cwd: t.TempDir()}, `echo "${STEPS_RUN_ID+set} ${STEPS_URL+set}"`)
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}

	if got := strings.TrimSpace(string(out)); got != "set" {
		t.Errorf("got %q, want only STEPS_RUN_ID set", got)
	}

	if got := BuildEnv(context.Background()); got != nil {
		t.Errorf("BuildEnv of a bare context = %v, want nil", got)
	}

	if got := buildEnvPairs(WithBuildMetadata(ctx, BuildMetadata{})); len(got) != 0 {
		t.Errorf("a zero BuildMetadata added %v", got)
	}
}

func TestBuildMetadataDropsAValueWithNUL(t *testing.T) {
	t.Parallel()

	ctx := WithBuildMetadata(context.Background(), BuildMetadata{RunID: "RUN1", JobName: "bad\x00name"})

	out, err := RunCapture(ctx, HostRunner{cwd: t.TempDir()}, `echo "$STEPS_RUN_ID ${STEPS_JOB_NAME+set}"`)
	if err != nil {
		t.Fatalf("a NUL must cost one variable, not the command: %v", err)
	}

	if got := strings.TrimSpace(string(out)); got != "RUN1" {
		t.Errorf("got %q", got)
	}
}

func TestContainerEnvPrecedenceAndOrder(t *testing.T) {
	t.Setenv("STEPS_T_HOST", "host")
	t.Setenv("STEPS_T_SHADOWED", "host")

	s := &dockerSession{
		envNames:  []string{"STEPS_T_HOST", "STEPS_T_SHADOWED", "STEPS_T_MISSING"},
		envValues: map[string]string{"STEPS_JOB_NAME": "supplied", "STEPS_T_SHADOWED": "supplied"},
	}

	got := s.containerEnv()
	want := []string{"STEPS_JOB_NAME=supplied", "STEPS_T_HOST=host", "STEPS_T_SHADOWED=supplied"}

	if !slices.Equal(got, want) {
		t.Errorf("containerEnv = %v, want %v", got, want)
	}
}
