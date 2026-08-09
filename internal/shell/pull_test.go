package shell

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// writeFakePullDocker installs a "docker" on PATH that records its argv and
// answers `image inspect` according to which images are considered present.
// Separate from writeFakeDocker because these tests care about a different
// pair of subcommands (inspect/pull) than the session tests do (run/exec/rm).
func writeFakePullDocker(t *testing.T, present []string, pullExit int) (argvFile string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake docker script assumes a POSIX shell")
	}

	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + argvFile + "\n" +
		"if [ \"$1\" = image ] && [ \"$2\" = inspect ]; then\n" +
		"  for p in $FAKE_PRESENT; do [ \"$p\" = \"$4\" ] && exit 0; done\n" +
		"  exit 1\n" +
		"fi\n" +
		"if [ \"$1\" = pull ]; then exit \"$FAKE_PULL_EXIT\"; fi\n" +
		"exit 0\n"

	err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700) //nolint:gosec // test fixture, needs to be executable
	if err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PRESENT", strings.Join(present, " "))
	t.Setenv("FAKE_PULL_EXIT", strconv.Itoa(pullExit))

	return argvFile
}

func pullArgv(t *testing.T, argvFile string) []string {
	t.Helper()

	data, err := os.ReadFile(argvFile) //nolint:gosec // test fixture path
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		t.Fatalf("read recorded argv: %v", err)
	}

	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestPrepareImagesPullsOnlyMissingOnes is the property that keeps this cheap
// enough to run before every job (including every job `steps watch` fires):
// an image already on the daemon costs one local inspect and no network.
func TestPrepareImagesPullsOnlyMissingOnes(t *testing.T) {
	argvFile := writeFakePullDocker(t, []string{"alpine:3"}, 0)

	err := PrepareImages(context.Background(), []string{"alpine:3", "golang:1.26"})
	if err != nil {
		t.Fatalf("PrepareImages: %v", err)
	}

	var pulls []string

	for _, line := range pullArgv(t, argvFile) {
		if strings.HasPrefix(line, "pull ") {
			pulls = append(pulls, line)
		}
	}

	if len(pulls) != 1 {
		t.Fatalf("pull invocations = %v, want exactly one (only the missing image)", pulls)
	}

	if !strings.Contains(pulls[0], "golang:1.26") {
		t.Errorf("pull argv = %q, want it to name the missing image", pulls[0])
	}
}

// TestPrepareImagesLocallyBuiltImageIsNotPulled covers the case the inspect
// check exists for: an image built on this machine exists in no registry, so
// pulling it would fail a run that would otherwise work.
func TestPrepareImagesLocallyBuiltImageIsNotPulled(t *testing.T) {
	argvFile := writeFakePullDocker(t, []string{"my-local-build:dev"}, 1)

	err := PrepareImages(context.Background(), []string{"my-local-build:dev"})
	if err != nil {
		t.Fatalf("PrepareImages: %v", err)
	}

	for _, line := range pullArgv(t, argvFile) {
		if strings.HasPrefix(line, "pull ") {
			t.Errorf("pulled a locally-present image: %q", line)
		}
	}
}

// TestPrepareImagesReportsAPullFailure pins that an unpullable image stops the
// run up front. Letting it through would just move the same failure into the
// first step that needed it, which is the whole thing being fixed.
func TestPrepareImagesReportsAPullFailure(t *testing.T) {
	writeFakePullDocker(t, nil, 1)

	err := PrepareImages(context.Background(), []string{"nope:latest"})
	if err == nil {
		t.Fatal("expected an error when the pull fails")
	}

	if !strings.Contains(err.Error(), "nope:latest") {
		t.Errorf("error = %v, want it to name the image", err)
	}
}

func TestPrepareImagesNoImagesDoesNothing(t *testing.T) {
	argvFile := writeFakePullDocker(t, nil, 0)

	err := PrepareImages(context.Background(), nil)
	if err != nil {
		t.Fatalf("PrepareImages: %v", err)
	}

	if argv := pullArgv(t, argvFile); len(argv) != 0 {
		t.Errorf("invoked docker for an empty image list: %v", argv)
	}
}

// TestPrepareImagesPassesImagesPositionally guards the same flag-smuggling
// defense dockerStartArgs has: an image value is separated from the flags by
// "--" so it can only ever be looked up as an image name.
func TestPrepareImagesPassesImagesPositionally(t *testing.T) {
	argvFile := writeFakePullDocker(t, nil, 0)

	err := PrepareImages(context.Background(), []string{"alpine:3"})
	if err != nil {
		t.Fatalf("PrepareImages: %v", err)
	}

	for _, line := range pullArgv(t, argvFile) {
		if !strings.Contains(line, "-- alpine:3") {
			t.Errorf("argv = %q, want the image behind a -- separator", line)
		}
	}
}
