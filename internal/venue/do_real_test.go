package venue

// Against real DigitalOcean, opt-in: `hack/do-fixture.sh up`, export what it prints and DIGITALOCEAN_ACCESS_TOKEN. The fakes cover the REST API and plain ssh; what only a real droplet shows is that cloud-init installs the minted host key and the login key for root, and how long the image's own key is offered first.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

func TestRealDOLaunchRungRunsAStepAndDeletes(t *testing.T) {
	region := os.Getenv("STEPS_TEST_DO_REGION")
	if region == "" || os.Getenv("DIGITALOCEAN_ACCESS_TOKEN") == "" {
		t.Skip("no DigitalOcean fixture — run hack/do-fixture.sh up, export what it prints and DIGITALOCEAN_ACCESS_TOKEN")
	}

	worker, err := ParseWorker("do://launch/?region=" + region)
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	leases := NewLeases(map[string]Worker{"do": worker})

	started := time.Now()

	resolved, err := leases.Resolve(context.Background(), "do")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	t.Cleanup(func() {
		err := leases.ReleaseAll(context.Background())
		if err != nil {
			t.Errorf("ReleaseAll: %v — droplet %s may still be billing; hack/do-fixture.sh down deletes it", err, resolved.Instance)
		}
	})

	t.Logf("droplet %s active with an address after %s", resolved.Instance, time.Since(started).Round(time.Second))

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, shell.RunnerSpec{Cwd: cwd, Image: "public.ecr.aws/docker/library/alpine:3", Worker: resolved.URL, Fetch: []string{"out"}})

	err = shell.Run(context.Background(), runner, "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("Run on a real droplet: %v", err)
	}

	t.Logf("first step done %s after the create", time.Since(started).Round(time.Second))

	if got := mustRead(t, filepath.Join(cwd, "out", "report.txt")); got != "seed\n" {
		t.Errorf("out/report.txt = %q, want %q", got, "seed\n")
	}
}
