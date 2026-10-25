package e2e

// The artifact bench (steps#225): every way a tree moves between steps, jobs and machines, each held to a byte and disk budget. `task bench:artifacts` runs it; hack/aws-fixture.sh and hack/gcp-fixture.sh supply the machines, and a tag with no fixture falls back to a local: worker so the bench itself can be run on a laptop.

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/testdocker"
)

const (
	// benchImage is multi-arch, from ECR so a worker pulling it meets no Docker Hub rate limit.
	benchImage = "public.ecr.aws/docker/library/alpine:3.20"
	// benchImageDigest is benchImage's index digest, the version the image-from-an-artifact get reports.
	benchImageDigest = "sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	benchWritten     = 1 << 20
	// benchOverhead is what tar headers and a step's top-level files may add to a hop's bytes.
	benchOverhead = 1 << 20
	// benchDiskSlack is the issue's budget past what a step wrote: empty volumes, overlay work dirs, du's rounding.
	benchDiskSlack = 4 << 20
	// benchS3Mock stands in for S3 when no bucket is named: MinIO no longer publishes images, and adobe/s3mock drops the aws CLI's streamed uploads.
	benchS3Mock = "chrislusf/seaweedfs:3.93"
)

type benchS3 struct {
	bucket, region, keyID, secret string
	// workerEndpoint is how a worker's container reaches the store, localEndpoint how this machine does; both empty for AWS itself.
	workerEndpoint, localEndpoint string
}

type benchEnv struct {
	workers map[string]string
	payload int64
	s3      benchS3
}

// hopRow is one line of the bench's table.
type hopRow struct {
	hop, route, how string
	wall            time.Duration
	bytes, disk     int64
	problems        []string
	// skipped says why a hop could not be measured here: a policy or a missing fixture, never the code being wrong.
	skipped string
}

func (r hopRow) passed() bool { return len(r.problems) == 0 }

// benchStep is what one placed step cost, keyed job/step.
type benchStep struct {
	placement store.Placement
	wall      time.Duration
}

// benchRun is what one invocation of the CLI left behind.
type benchRun struct {
	err   error
	steps map[string]benchStep
}

func TestBenchArtifacts(t *testing.T) {
	if os.Getenv("STEPS_BENCH_ARTIFACTS") == "" {
		t.Skip("the artifact bench runs only on request: task bench:artifacts")
	}

	env := benchEnvironment(t)
	nonce := strings.ToLower(rand.Text()[:12])
	dir := t.TempDir()

	t.Logf("payload %s, workers %v, bucket %s, nonce %s", formatBytes(env.payload), env.workers, env.s3.bucket, nonce)
	t.Cleanup(func() { benchRemovePrefix(t, env.s3, "steps-bench/"+nonce+"/") })

	path := writePipeline(t, dir, benchPipeline(env, nonce))

	jobs := []string{"relay", "publish", "consume", "features", "warm"}
	first := benchInvoke(t, path, env, jobs, "--var", "round=1")
	second := benchInvoke(t, path, env, jobs, "--var", "round=2")

	rows := judgeBench(env.payload, first, second)
	rows = append(rows, benchArtifactStore(t, env, nonce)...)
	rows = append(rows, benchDrains(t, env, nonce)...)

	t.Log("\n" + formatHops(rows))

	for _, row := range rows {
		if !row.passed() {
			t.Errorf("hop %s %s: %s", row.hop, row.route, strings.Join(row.problems, "; "))
		}
	}
}

// benchEnvironment resolves each tag to a fixture's machine, or a local: worker when that cloud has none.
func benchEnvironment(t *testing.T) benchEnv {
	t.Helper()

	roots := t.TempDir()
	env := benchEnv{payload: benchPayload(t), workers: map[string]string{
		"aws": cmp.Or(os.Getenv("STEPS_BENCH_AWS_WORKER"), awsFixtureWorker(), "local:"+filepath.Join(roots, "aws")),
		"gcp": cmp.Or(os.Getenv("STEPS_BENCH_GCP_WORKER"), gcpFixtureWorker(), "local:"+filepath.Join(roots, "gcp")),
	}}

	for _, worker := range env.workers {
		if strings.HasPrefix(worker, "local:") {
			testdocker.Require(t)
		}
	}

	bucket := cmp.Or(os.Getenv("STEPS_BENCH_BUCKET"), os.Getenv("STEPS_TEST_AWS_BUCKET"))
	if bucket == "" {
		env.s3 = startS3Mock(t)

		return env
	}

	// The workers hold no credentials of their own (the fixture's instance profile is SSM only), so these travel to them as pipeline vars.
	env.s3 = benchS3{
		bucket: bucket, region: cmp.Or(os.Getenv("STEPS_TEST_AWS_REGION"), os.Getenv("AWS_REGION"), "us-east-1"),
		keyID: os.Getenv("AWS_ACCESS_KEY_ID"), secret: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	if env.s3.keyID == "" || env.s3.secret == "" || os.Getenv("AWS_SESSION_TOKEN") != "" {
		t.Fatal(`the bench needs long-lived AWS keys in AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY to hand its workers: eval "$(aws configure export-credentials --profile cli --format env)"`)
	}

	return env
}

func awsFixtureWorker() string {
	instance := os.Getenv("STEPS_TEST_AWS_INSTANCE")
	if instance == "" {
		return ""
	}

	if region := os.Getenv("STEPS_TEST_AWS_REGION"); region != "" {
		return "aws://" + instance + "?region=" + region
	}

	return "aws://" + instance
}

func gcpFixtureWorker() string {
	instance := os.Getenv("STEPS_TEST_GCP_INSTANCE")
	if instance == "" {
		return ""
	}

	return "gcp://" + instance + "?project=" + os.Getenv("STEPS_TEST_GCP_PROJECT") + "&zone=" + os.Getenv("STEPS_TEST_GCP_ZONE")
}

// benchPayload is PAYLOAD (bytes, or with a K/M/G suffix), 256MiB by default: one accidental copy of that blows any budget here.
func benchPayload(t *testing.T) int64 {
	t.Helper()

	raw := cmp.Or(os.Getenv("PAYLOAD"), "256M")
	shift := map[byte]int{'K': 10, 'M': 20, 'G': 30}[raw[len(raw)-1]]

	if shift > 0 {
		raw = raw[:len(raw)-1]
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		t.Fatalf("PAYLOAD=%q: want a positive size like 268435456 or 256M", os.Getenv("PAYLOAD"))
	}

	return n << shift
}

// startS3Mock runs an S3 stand-in on this machine's daemon, reached by workers on its bridge address and by this process on a published port.
func startS3Mock(t *testing.T) benchS3 {
	t.Helper()
	testdocker.Require(t)

	name := "steps-bench-s3-" + strings.ToLower(rand.Text()[:8])
	benchCommand(t, "docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::8333", benchS3Mock, "server", "-s3", "-dir=/data")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		// -v: the image declares a VOLUME, and without it every run leaves gigabytes of store behind.
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).Run() //nolint:gosec // a name this test minted
	})

	address := strings.TrimSpace(benchCommand(t, "docker", "port", name, "8333/tcp"))
	ip := strings.TrimSpace(benchCommand(t, "docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name))

	s3 := benchS3{
		bucket: "bench", region: "us-east-1", keyID: "bench", secret: "bench",
		workerEndpoint: "http://" + ip + ":8333", localEndpoint: "http://" + address,
	}

	t.Setenv("AWS_ACCESS_KEY_ID", s3.keyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", s3.secret)
	t.Setenv("AWS_SESSION_TOKEN", "")

	deadline := time.Now().Add(time.Minute)
	for benchTryAWS(s3, "s3api", "create-bucket", "--bucket", s3.bucket) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("the S3 stand-in at %s never answered", s3.localEndpoint)
		}

		time.Sleep(time.Second)
	}

	return s3
}

func benchCommand(t *testing.T, name string, args ...string) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), name, args...).Output() //nolint:gosec // the bench's own commands
	if err != nil {
		t.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}

	return string(out)
}

func benchAWSArgs(s3 benchS3, args ...string) []string {
	base := []string{"--region", s3.region}
	if s3.localEndpoint != "" {
		base = append(base, "--endpoint-url", s3.localEndpoint)
	}

	return append(base, args...)
}

func benchTryAWS(s3 benchS3, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := exec.CommandContext(ctx, "aws", benchAWSArgs(s3, args...)...).Run() //nolint:gosec // the bench's own commands
	if err != nil {
		return fmt.Errorf("aws %s: %w", strings.Join(args, " "), err)
	}

	return nil
}

// benchRemovePrefix deletes what the bench put in the bucket, under its own bound: the likeliest reason it runs is that the test just failed.
func benchRemovePrefix(t *testing.T, s3 benchS3, prefix string) {
	t.Helper()

	err := benchTryAWS(s3, "s3", "rm", "--recursive", "--quiet", "s3://"+s3.bucket+"/"+prefix)
	if err != nil {
		t.Logf("could not empty s3://%s/%s: %v — remove it by hand", s3.bucket, prefix, err)
	}
}

// benchCountObjects counts what sits under a prefix.
func benchCountObjects(t *testing.T, s3 benchS3, prefix string) int {
	t.Helper()

	out := benchCommand(t, "aws", benchAWSArgs(s3, "s3api", "list-objects-v2", "--bucket", s3.bucket, "--prefix", prefix,
		"--query", "length(Contents || `[]`)", "--output", "text")...)

	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("counting s3://%s/%s: %q", s3.bucket, prefix, out)
	}

	return n
}

// benchInvoke runs each job in turn, as one-shot `steps run` does one job, and reads back what each placed step cost; the first failure ends the walk, since later jobs read what earlier ones published.
func benchInvoke(t *testing.T, path string, env benchEnv, jobs []string, extra ...string) benchRun {
	t.Helper()

	args := []string{path,
		"--var", "s3_endpoint=" + env.s3.workerEndpoint, "--var", "s3_key_id=" + env.s3.keyID, "--var", "s3_secret=" + env.s3.secret}
	for tag, worker := range env.workers {
		args = append(args, "--worker", tag+"="+worker)
	}

	started := time.Now()

	var err error

	for _, job := range jobs {
		out := captureStdout(t, func() { err = cli.Run(append(append(args, "--job", job), extra...)) })
		if err != nil {
			err = fmt.Errorf("job %s: %w", job, err)
			t.Logf("%v\n%s", err, out)

			break
		}
	}

	return benchRun{err: err, steps: benchSteps(t, path, started)}
}

// benchSteps reads every run that began at or after started: one per job.
func benchSteps(t *testing.T, path string, started time.Time) map[string]benchStep {
	t.Helper()

	ctx := context.Background()
	st := openStoreFor(t, path)

	runs, err := st.ListRuns(ctx, "", 100)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}

	steps := map[string]benchStep{}

	for _, run := range runs {
		if run.StartedAt.Before(started.Add(-time.Second)) {
			continue
		}

		placements, err := st.RunPlacements(ctx, run.ID)
		if err != nil {
			t.Fatalf("placements of %s: %v", run.ID, err)
		}

		walls := map[string]time.Duration{}

		events, err := st.RunEvents(ctx, run.ID, 0, 100_000)
		if err != nil {
			t.Fatalf("events of %s: %v", run.ID, err)
		}

		for _, event := range events {
			if event.Type == "step_finished" {
				walls[event.StepName] = time.Duration(event.DurationMS) * time.Millisecond
			}
		}

		for _, placement := range placements {
			// A hook's row carries its scope, not a node: it is told apart by its own name, which is the hook step's.
			steps[placement.JobName+"/"+placement.StepName] = benchStep{placement: placement, wall: walls[placement.StepName]}
		}
	}

	_ = st.Close()

	return steps
}

// benchSeed writes payload bytes of noise, the nonce, and their digest, so a consumer can tell this run's tree from any other.
func benchSeed(dir string, payload int64, nonce string) string {
	return fmt.Sprintf(`set -eo pipefail
head -c %d /dev/urandom > %[2]s/blob.bin
printf '%%s' %[3]s > %[2]s/nonce.txt
cat %[2]s/blob.bin %[2]s/nonce.txt | sha256sum | cut -d' ' -f1 > %[2]s/sum.txt`, payload, dir, nonce)
}

// benchVerify refuses a tree that is not this run's, then writes benchWritten bytes of its own.
func benchVerify(dir, out, nonce string) string {
	return fmt.Sprintf(`set -eo pipefail
test "$(cat %[1]s/nonce.txt)" = %[3]s || { echo "stale tree in %[1]s: nonce $(cat %[1]s/nonce.txt)" >&2; exit 1; }
test "$(cat %[1]s/blob.bin %[1]s/nonce.txt | sha256sum | cut -d' ' -f1)" = "$(cat %[1]s/sum.txt)" || { echo "%[1]s does not match its digest" >&2; exit 1; }
head -c %[4]d /dev/urandom > %[2]s/note.bin`, dir, out, nonce, benchWritten)
}

func indent(script, by string) string {
	return by + strings.ReplaceAll(script, "\n", "\n"+by)
}

// benchPipeline is five jobs: relay carries the in-job hops, publish and consume the between-jobs one, features exercises the rest of the placeable DSL on a tiny tree, and warm is the task cache. Relay has no put, so a rerun can skip it whole; a put makes its chain unskippable (docs/workspace.md), which is why publish is a job of its own. Left out, as unplaceable or out of scope: agent steps and ensemble: (agent-only), fix: and mcp:/expr: types (refused with tags), approval: and load_var: (place nothing), cache.resources: (this machine's disk), privileged: (moves no tree).
func benchPipeline(env benchEnv, nonce string) string {
	p := env.payload
	cache := max(p/16, 1<<20)
	s3setup := `set -eo pipefail
      apk add -q --no-cache aws-cli >&2
      export AWS_ACCESS_KEY_ID={{ .source.key_id | shellquote }} AWS_SECRET_ACCESS_KEY={{ .source.secret | shellquote }} AWS_DEFAULT_REGION={{ .source.region | shellquote }}
      endpoint={{ .source.endpoint | shellquote }}; [ -z "$endpoint" ] || export AWS_ENDPOINT_URL="$endpoint"
      bucket={{ .source.bucket | shellquote }}`

	source := func(from string) string {
		return fmt.Sprintf(`{bucket: %s, region: %s, endpoint: "((s3_endpoint))", key_id: "((s3_key_id))", secret: "((s3_secret))", prefix: steps-bench/%s/%s/}`,
			env.s3.bucket, env.s3.region, nonce, from)
	}

	task := func(name, tags, inputs, outputs, run string) string {
		step := "  - task: " + name + "\n"
		if tags != "" {
			step += "    tags: [" + tags + "]\n"
		}

		step += "    image: " + benchImage + "\n"
		if inputs != "" {
			step += "    inputs: [" + inputs + "]\n"
		}

		if outputs != "" {
			step += "    outputs: [" + outputs + "]\n"
		}

		return step + "    run: |\n" + indent(run, "      ") + "\n"
	}

	warm := func(tag string) string {
		return fmt.Sprintf(`  - task: warm-%[1]s
    tags: [%[1]s]
    image: %[2]s
    volatile: true
    caches:
    - path: cache
    outputs: [warm-%[1]s]
    run: |
      set -e
      if [ "((round))" = 2 ]; then
        grep -qx %[3]s-1 cache/marker && test -s cache/blob.bin || { echo "the cache did not survive: $(cat cache/marker 2>/dev/null)" >&2; exit 1; }
      else
        rm -f cache/marker && head -c %[4]d /dev/urandom > cache/blob.bin
      fi
      echo %[3]s-((round)) >> cache/marker
      echo ok > warm-%[1]s/ok
`, tag, benchImage, nonce, cache)
	}

	return `resource_types:
- name: s3tree
  image: ` + benchImage + `
  config:
    check: |
      ` + s3setup + `
      aws s3api list-objects-v2 --bucket "$bucket" --prefix {{ .source.prefix | shellquote }} --query 'sort_by(Contents || ` + "`[]`" + `, &LastModified)[].{key: Key}' --output json
    in: |
      ` + s3setup + `
      aws s3 cp --quiet "s3://$bucket/"{{ .version.key | shellquote }} - | tar -xf -
    out: |
      ` + s3setup + `
      key={{ .source.prefix | shellquote }}tree.tar
      tar -cf - -C {{ .params.from | shellquote }} . | aws s3 cp --quiet - "s3://$bucket/$key"
      printf '{"key":"%s"}' "$key"
- name: digest
  image: ` + benchImage + `
  config:
    check: printf '[{"digest":"` + benchImageDigest + `"}]'
    in: printf '%s' {{ .version.digest | shellquote }} > digest

resources:
- name: s3-from-aws
  type: s3tree
  tags: [aws]
  source: ` + source("from-aws") + `
- name: s3-from-gcp
  type: s3tree
  tags: [gcp]
  source: ` + source("from-gcp") + `
- name: pinned-alpine
  type: digest
  tags: [gcp]
  source: {repository: public.ecr.aws/docker/library/alpine}

tasks:
- name: verify-mapped
  image: ` + benchImage + `
  inputs: [payload]
  outputs: [out]
  run: |
` + indent(benchVerify("payload", "out", nonce), "    ") + `

jobs:
- name: relay
  plan:
  - in_parallel:
    - task: seed-aws
      tags: [aws]
      image: ` + benchImage + `
      outputs: [payload-aws]
      run: |
` + indent(benchSeed("payload-aws", p, nonce), "        ") + `
    - task: seed-gcp
      tags: [gcp]
      image: ` + benchImage + `
      outputs: [payload-gcp]
      run: |
` + indent(benchSeed("payload-gcp", p, nonce), "        ") + `
  - tags: [aws]
    do:
` + indent(strings.TrimSuffix(task("same-aws", "", "payload-aws", "same-aws", benchVerify("payload-aws", "same-aws", nonce)), "\n"), "  ") + `
` + task("same-gcp", "gcp", "payload-gcp", "same-gcp", benchVerify("payload-gcp", "same-gcp", nonce)) +
		task("cross-gcp", "gcp", "payload-aws", "cross-gcp", benchVerify("payload-aws", "cross-gcp", nonce)) +
		task("cross-aws", "aws", "payload-gcp", "cross-aws", benchVerify("payload-gcp", "cross-aws", nonce)) + `
- name: publish
  plan:
` + task("pub-aws", "aws", "", "pub-aws", benchSeed("pub-aws", p, nonce)) +
		task("pub-gcp", "gcp", "", "pub-gcp", benchSeed("pub-gcp", p, nonce)) + `  - put: s3-from-aws
    inputs: [pub-aws]
    params: {from: pub-aws}
  - put: s3-from-gcp
    inputs: [pub-gcp]
    params: {from: pub-gcp}

- name: consume
  tags: [gcp]
  plan:
  - get: s3-from-aws
    tags: [gcp]
    passed: [publish]
  - get: s3-from-gcp
    tags: [aws]
    passed: [publish]
` + task("tree-gcp", "", "s3-from-aws", "tree-gcp", benchVerify("s3-from-aws", "tree-gcp", nonce)) +
		task("tree-aws", "aws", "s3-from-gcp", "tree-aws", benchVerify("s3-from-gcp", "tree-aws", nonce)) + `
- name: features
  plan:
  - get: pinned-alpine
` + task("tiny", "aws", "", "tiny", benchSeed("tiny", 64<<10, nonce)) + `  - task: verify-mapped
    tags: [aws]
    input_mapping: {payload: tiny}
    output_mapping: {out: mapped}
  - try:
      task: tolerated
      tags: [gcp]
      image: ` + benchImage + `
      run: "false"
  - across:
    - var: n
      values: ["1", "2"]
    max_in_flight: 2
    task: fan
    tags: [gcp]
    image: ` + benchImage + `
    inputs: [tiny]
    run: test -s tiny/blob.bin
  - task: knobs
    tags: [aws]
    image: ` + benchImage + `
    inputs: [tiny]
    when: test -s tiny/blob.bin
    attempts: 2
    timeout: 5m
    user: nobody
    network: none
    container_limits: {memory: 268435456}
    env: [STEPS_BENCH_ARTIFACTS]
    run: test -n "$STEPS_BENCH_ARTIFACTS" && test "$(id -un)" = nobody && test -s tiny/blob.bin
  - race:
      steps:
      - task: fast
        tags: [aws]
        image: ` + benchImage + `
        outputs: [won]
        run: echo fast > won/who
      - task: slow
        tags: [aws]
        image: ` + benchImage + `
        outputs: [won]
        run: sleep 5 && echo slow > won/who
  - task: pinned
    tags: [gcp]
    image: pinned-alpine
    inputs: [won, mapped]
    run: grep -q '^3\.20' /etc/alpine-release && test -s mapped/note.bin && cat won/who
    on_failure:
      task: never
      image: ` + benchImage + `
      run: "true"
    ensure:
      task: hooked
      image: ` + benchImage + `
      run: "true"

- name: warm
  plan:
` + warm("aws") + warm("gcp")
}

// judgeBench holds the two runs to the issue's budgets. Pure, so each budget can be proven to fail without a cloud.
func judgeBench(payload int64, first, second benchRun) []hopRow {
	features := hopRow{hop: "-", route: "features", how: "do/in_parallel/try/across/race/hooks/mapping/image"}
	if first.err != nil {
		features.problems = append(features.problems, "first run failed: "+first.err.Error())
	}

	if second.err != nil {
		features.problems = append(features.problems, "second run failed: "+second.err.Error())
	}

	return []hopRow{
		benchHop(first, "1", "aws→aws", "overlay", []benchExpect{{key: "relay/same-aws", diskLow: benchWritten, diskHigh: benchWritten + benchDiskSlack}}),
		benchHop(first, "1", "gcp→gcp", "overlay", []benchExpect{{key: "relay/same-gcp", diskLow: benchWritten, diskHigh: benchWritten + benchDiskSlack}}),
		// A relay pulls the tree off the holder and pours it into the consumer: twice the payload over this machine's link, once each way, and a filled cache entry on the consumer.
		benchHop(first, "2", "aws→gcp", "relay", []benchExpect{{key: "relay/cross-gcp", sentLow: 2 * payload, sentHigh: 2*payload + benchOverhead, diskLow: payload + benchWritten, diskHigh: payload + benchWritten + benchDiskSlack}}),
		benchHop(first, "2", "gcp→aws", "relay", []benchExpect{{key: "relay/cross-aws", sentLow: 2 * payload, sentHigh: 2*payload + benchOverhead, diskLow: payload + benchWritten, diskHigh: payload + benchWritten + benchDiskSlack}}),
		benchHop(first, "3", "aws→S3→gcp", "bucket", benchBucketHop("s3-from-aws", "tree-gcp", payload)),
		benchHop(first, "3", "gcp→S3→aws", "bucket", benchBucketHop("s3-from-gcp", "tree-aws", payload)),
		benchRerun(second, payload),
		benchHop(first, "5", "aws cache", "cache", []benchExpect{{key: "warm/warm-aws", diskHigh: benchDiskSlack}}),
		benchHop(second, "5", "aws cache", "cache hit", []benchExpect{{key: "warm/warm-aws", diskHigh: benchDiskSlack}}),
		benchHop(first, "5", "gcp cache", "cache", []benchExpect{{key: "warm/warm-gcp", diskHigh: benchDiskSlack}}),
		benchHop(second, "5", "gcp cache", "cache hit", []benchExpect{{key: "warm/warm-gcp", diskHigh: benchDiskSlack}}),
		features,
	}
}

// benchExpect is one placed step's budget; a zero high bound on bytes means benchOverhead, on disk benchDiskSlack.
type benchExpect struct {
	// unweighed is a step that holds no output on its worker, whose disk nothing measures.
	unweighed         bool
	key               string
	sentLow, sentHigh int64
	diskLow, diskHigh int64
	receivedHigh      int64
}

func benchHop(run benchRun, hop, route, how string, expects []benchExpect) hopRow {
	row := hopRow{hop: hop, route: route, how: how}

	for _, want := range expects {
		step, ok := run.steps[want.key]
		if !ok {
			row.problems = append(row.problems, want.key+" did not run on a worker")

			continue
		}

		placed := step.placement
		row.wall += step.wall
		row.bytes += placed.BytesSent + placed.BytesReceived

		sentHigh := cmp.Or(want.sentHigh, benchOverhead)
		if placed.BytesSent < want.sentLow || placed.BytesSent > sentHigh {
			row.problems = append(row.problems, fmt.Sprintf("%s sent %s, want %s..%s", want.key, formatBytes(placed.BytesSent), formatBytes(want.sentLow), formatBytes(sentHigh)))
		}

		if received := cmp.Or(want.receivedHigh, benchOverhead); placed.BytesReceived > received {
			row.problems = append(row.problems, fmt.Sprintf("%s brought %s home, want at most %s", want.key, formatBytes(placed.BytesReceived), formatBytes(received)))
		}

		if placed.VolumeBytes == nil {
			if !want.unweighed {
				row.problems = append(row.problems, want.key+"'s disk was not weighed")
			}

			continue
		}

		disk := *placed.VolumeBytes
		row.disk += disk

		diskHigh := cmp.Or(want.diskHigh, benchDiskSlack)
		if disk < want.diskLow || disk > diskHigh {
			row.problems = append(row.problems, fmt.Sprintf("%s grew its worker's disk by %s, want %s..%s", want.key, formatBytes(disk), formatBytes(want.diskLow), formatBytes(diskHigh)))
		}
	}

	return row
}

// benchBucketHop is the Concourse way between jobs: the put streams the tree from its worker to the bucket, the get streams it onto the other worker, and this machine sees none of it.
func benchBucketHop(resource, consumer string, payload int64) []benchExpect {
	return []benchExpect{
		{key: "publish/" + resource, unweighed: true},
		{key: "consume/" + resource, diskLow: payload, diskHigh: payload + benchDiskSlack},
		{key: "consume/" + consumer, diskLow: benchWritten, diskHigh: benchWritten + benchDiskSlack},
	}
}

// benchRerun: nothing changed, so relay and consume are skipped whole and place nothing, while publish runs again because its puts make its chain unskippable — and still moves nothing through here, every tree staying on the worker that made it. A one-shot run checks each resource before the plan, so the checks run too.
func benchRerun(second benchRun, payload int64) hopRow {
	row := hopRow{hop: "4", route: "rerun", how: "cache hit"}

	for key, step := range second.steps {
		job, name, _ := strings.Cut(key, "/")
		if job != "relay" && job != "consume" && job != "publish" {
			continue
		}

		placed := step.placement
		row.wall += step.wall
		row.bytes += placed.BytesSent + placed.BytesReceived

		if job != "publish" && !strings.HasPrefix(name, "check ") {
			row.problems = append(row.problems, key+" ran again with nothing changed")
		}

		if placed.BytesSent+placed.BytesReceived > benchOverhead {
			row.problems = append(row.problems, fmt.Sprintf("%s moved %s", key, formatBytes(placed.BytesSent+placed.BytesReceived)))
		}

		benchRerunDisk(&row, key, name, placed, payload)
	}

	if second.err != nil {
		row.problems = append(row.problems, "the rerun failed: "+second.err.Error())
	}

	return row
}

// benchRerunDisk: a re-run seed writes its tree afresh; anything else writing more than the slack copied something.
func benchRerunDisk(row *hopRow, key, name string, placed store.Placement, payload int64) {
	if placed.VolumeBytes == nil {
		return
	}

	budget := int64(benchDiskSlack)
	if strings.HasPrefix(name, "pub-") {
		budget += payload
	}

	row.disk += *placed.VolumeBytes

	if *placed.VolumeBytes > budget {
		row.problems = append(row.problems, fmt.Sprintf("%s grew its worker's disk by %s, want at most %s", key, formatBytes(*placed.VolumeBytes), formatBytes(budget)))
	}
}

// benchArtifactStore is hop 6: the store is the step cache's remote half, so a machine that kept only the state database skips every step and gets the outputs back from the bucket.
func benchArtifactStore(t *testing.T, env benchEnv, nonce string) []hopRow {
	t.Helper()

	payload := min(env.payload, 32<<20)
	dir := t.TempDir()
	root := filepath.Join(dir, "ws-root")
	prefix := "steps-bench/" + nonce + "/store"

	store := "s3://" + env.s3.bucket + "/" + prefix + "?region=" + env.s3.region
	if env.s3.localEndpoint != "" {
		store += "&endpoint=" + env.s3.localEndpoint
	}

	path := writePipeline(t, dir, `
workspace:
  root: `+root+`

jobs:
- name: stored
  plan:
  - task: stored-seed
    tags: [aws]
    image: `+benchImage+`
    outputs: [payload]
    run: |
`+indent(benchSeed("payload", payload, nonce), "      ")+`
  - task: stored-verify
    tags: [gcp]
    image: `+benchImage+`
    inputs: [payload]
    outputs: [out]
    run: |
`+indent(benchVerify("payload", "out", nonce), "      ")+`
  - task: stored-read
    inputs: [out]
    run: test -s out/note.bin
`)

	first := benchInvoke(t, path, env, []string{"stored"}, "--artifact-store", store)

	err := os.RemoveAll(root)
	if err != nil {
		t.Fatalf("removing the workspace root: %v", err)
	}

	second := benchInvoke(t, path, env, []string{"stored"}, "--artifact-store", store)

	row := hopRow{hop: "6", route: "store→here", how: "artifact store"}

	if first.err != nil || second.err != nil {
		row.problems = append(row.problems, fmt.Sprintf("runs failed: %v / %v", first.err, second.err))
	}

	for key, step := range second.steps {
		row.problems = append(row.problems, key+" re-ran with only the database kept")
		row.bytes += step.placement.BytesSent + step.placement.BytesReceived
	}

	// Two outputs, payload and out, each one blob however many times it was mirrored.
	if blobs := benchCountObjects(t, env.s3, prefix+"/blobs/"); blobs != 2 {
		row.problems = append(row.problems, fmt.Sprintf("the store holds %d blobs, want 2", blobs))
	}

	return []hopRow{row}
}

func formatHops(rows []hopRow) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%-4s %-13s %-14s %9s %10s %10s  %s\n", "HOP", "ROUTE", "HOW", "WALL", "BYTES", "DISK", "RESULT")

	for _, row := range rows {
		result := "pass"
		if row.skipped != "" {
			result = "skip: " + row.skipped
		}

		if !row.passed() {
			result = "FAIL: " + strings.Join(row.problems, "; ")
		}

		fmt.Fprintf(&b, "%-4s %-13s %-14s %9s %10s %10s  %s\n", row.hop, row.route, truncateHow(row.how), row.wall.Round(100*time.Millisecond), formatBytes(row.bytes), formatBytes(row.disk), result)
	}

	return b.String()
}

func truncateHow(how string) string {
	if len(how) > 14 {
		return how[:13] + "…"
	}

	return how
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	}

	return strconv.FormatInt(n, 10) + "B"
}

// benchDrain is a machine steps acquires for the job and a way to reclaim it from under a running step.
type benchDrain struct {
	cloud, worker, skip string
	reclaim             func(t *testing.T, instance string)
}

// benchDrains is hop 7. Only an acquired machine can be re-placed (docs/infra.md), so this needs the fixtures' launch templates, never the static instances.
func benchDrains(t *testing.T, env benchEnv, nonce string) []hopRow {
	t.Helper()

	rows := make([]hopRow, 0, 2)

	for _, drain := range []benchDrain{gcpDrain(), awsDrain()} {
		row := hopRow{hop: "7", route: drain.cloud + " drain", how: "re-place"}
		if drain.skip != "" {
			row.skipped = drain.skip
		} else {
			benchDrainRow(t, env, nonce, drain, &row)
		}

		rows = append(rows, row)
	}

	return rows
}

func gcpDrain() benchDrain {
	project, zone, template := os.Getenv("STEPS_TEST_GCP_PROJECT"), os.Getenv("STEPS_TEST_GCP_ZONE"), os.Getenv("STEPS_TEST_GCP_TEMPLATE")
	if template == "" {
		return benchDrain{cloud: "gcp", skip: "no STEPS_TEST_GCP_TEMPLATE (hack/gcp-fixture.sh up)"}
	}

	// On a spot instance a simulated maintenance event is a preemption, and the fixture's template deletes what it preempts.
	return benchDrain{cloud: "gcp", worker: "gcp://launch/" + template + "?project=" + project + "&zone=" + zone, reclaim: func(t *testing.T, instance string) {
		t.Helper()
		benchCommand(t, "gcloud", "compute", "instances", "simulate-maintenance-event", instance, "--project", project, "--zone", zone, "--async")
	}}
}

func awsDrain() benchDrain {
	region, template, role := os.Getenv("STEPS_TEST_AWS_REGION"), os.Getenv("STEPS_TEST_AWS_TEMPLATE"), os.Getenv("STEPS_TEST_AWS_FIS_ROLE")
	if template == "" || role == "" {
		return benchDrain{cloud: "aws", skip: "no STEPS_TEST_AWS_TEMPLATE / STEPS_TEST_AWS_FIS_ROLE (hack/aws-fixture.sh up)"}
	}

	// Spot only: FIS refuses to interrupt an on-demand instance.
	return benchDrain{cloud: "aws", worker: "aws://launch/" + template + "?capacity=spot&region=" + region, reclaim: func(t *testing.T, instance string) {
		t.Helper()
		benchInterruptSpot(t, region, role, instance)
	}}
}

// benchDrainRow reclaims the machine the producer ran on while the consumer runs there. The consumer is evicted, re-placed on a fresh machine that holds nothing, and must fail naming the tree and the machine that had it; the run after, with nothing reclaimed, recovers.
func benchDrainRow(t *testing.T, env benchEnv, nonce string, drain benchDrain, row *hopRow) {
	t.Helper()

	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: drain
  plan:
  - task: drain-seed
    tags: [spot]
    image: `+benchImage+`
    outputs: [payload]
    run: |
`+indent(benchSeed("payload", min(env.payload, 16<<20), nonce), "      ")+`
  - task: drain-read
    tags: [spot]
    image: `+benchImage+`
    inputs: [payload]
    outputs: [out]
    run: |
      sleep ((hold))
`+indent(benchVerify("payload", "out", nonce), "      ")+`
`)
	args := []string{path, "--worker", "spot=" + drain.worker}

	// Long enough to outlast EC2's two-minute notice and GCE's thirty seconds, with the reclaim's own lag on top.
	done := make(chan error, 1)
	started := time.Now()

	go func() { done <- cli.Run(append(args, "--var", "hold=420")) }()

	instance, err := benchAwaitInstance(t, path, "drain-seed", done)
	if err != nil {
		if strings.Contains(err.Error(), "UnauthorizedOperation") || strings.Contains(err.Error(), "AccessDenied") {
			row.skipped = "the account refuses the acquisition: " + err.Error()
		} else {
			row.problems = append(row.problems, "acquiring: "+err.Error())
		}

		return
	}

	// Every machine acquired is named, so a leak is at least findable; steps gives it back as the job ends.
	t.Logf("%s drain: producer ran on %s, reclaiming it", drain.cloud, instance)
	drain.reclaim(t, instance)

	var first error

	select {
	case first = <-done:
	case <-time.After(30 * time.Minute):
		t.Fatalf("%s drain: the run outlived thirty minutes — check for a running instance %s", drain.cloud, instance)
	}

	row.wall = time.Since(started)

	if first == nil {
		row.problems = append(row.problems, "the consumer passed with its producer's machine reclaimed")
	} else {
		for _, want := range []string{`"payload"`, instance} {
			if !strings.Contains(first.Error(), want) {
				row.problems = append(row.problems, fmt.Sprintf("the failure does not name %s: %v", want, first))
			}
		}
	}

	second := cli.Run(append(args, "--var", "hold=0"))
	if second != nil {
		row.problems = append(row.problems, "the run after the reclaim did not recover: "+second.Error())
	}
}

// benchAwaitInstance waits for the producer's placement and answers the instance it ran on, or the run's error if it ended first.
func benchAwaitInstance(t *testing.T, path, step string, done chan error) (string, error) {
	t.Helper()

	deadline := time.After(15 * time.Minute)

	for {
		select {
		case err := <-done:
			done <- err

			if err != nil {
				return "", err
			}

			return "", fmt.Errorf("the run ended before %s was recorded", step)
		case <-deadline:
			return "", fmt.Errorf("%s was never placed", step)
		case <-time.After(2 * time.Second):
		}

		if instance := benchPlacedInstance(path, step); instance != "" {
			return instance, nil
		}
	}
}

func benchPlacedInstance(path, step string) string {
	st, err := sqlite.OpenExisting(string(cli.StatePath(path, "")), cli.PipelineName(path))
	if err != nil {
		return ""
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	runs, err := st.ListRuns(ctx, "", 1)
	if err != nil || len(runs) == 0 {
		return ""
	}

	placements, _ := st.RunPlacements(ctx, runs[0].ID)
	for _, placement := range placements {
		if placement.StepName == step && placement.InstanceID != nil {
			return *placement.InstanceID
		}
	}

	return ""
}

// benchInterruptSpot asks FIS to reclaim a spot instance, as internal/venue's TestRealAWSSpotEviction does.
func benchInterruptSpot(t *testing.T, region, role, instance string) {
	t.Helper()

	account := strings.TrimSpace(benchCommand(t, "aws", "--region", region, "sts", "get-caller-identity", "--query", "Account", "--output", "text"))
	template := `{"description": "steps artifact bench", "roleArn": "` + role + `", "stopConditions": [{"source": "none"}],
"targets": {"victim": {"resourceType": "aws:ec2:spot-instance", "resourceArns": ["arn:aws:ec2:` + region + `:` + account + `:instance/` + instance + `"], "selectionMode": "ALL"}},
"actions": {"interrupt": {"actionId": "aws:ec2:send-spot-instance-interruptions", "parameters": {"durationBeforeInterruption": "PT2M"}, "targets": {"SpotInstances": "victim"}}}}`

	file := filepath.Join(t.TempDir(), "fis.json")

	err := os.WriteFile(file, []byte(template), 0o600) //nolint:gosec // a file in this test's own temp dir
	if err != nil {
		t.Fatal(err)
	}

	id := strings.TrimSpace(benchCommand(t, "aws", "--region", region, "fis", "create-experiment-template", "--cli-input-json", "file://"+file,
		"--query", "experimentTemplate.id", "--output", "text"))

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		_ = exec.CommandContext(ctx, "aws", "--region", region, "fis", "delete-experiment-template", "--id", id).Run() //nolint:gosec // an id this test minted
	})

	benchCommand(t, "aws", "--region", region, "fis", "start-experiment", "--experiment-template-id", id)
}

// benchPassing is a pair of runs inside every budget, for the judgment's tests to break one budget at a time.
func benchPassing(payload int64) (benchRun, benchRun) {
	at := func(sent, disk int64) benchStep {
		return benchStep{placement: store.Placement{BytesSent: sent, VolumeBytes: &disk}}
	}

	first := benchRun{steps: map[string]benchStep{
		"relay/same-aws": at(0, benchWritten), "relay/same-gcp": at(0, benchWritten),
		"relay/cross-gcp": at(2*payload, payload+benchWritten), "relay/cross-aws": at(2*payload, payload+benchWritten),
		"publish/s3-from-aws": at(0, 0), "publish/s3-from-gcp": at(0, 0),
		"consume/s3-from-aws": at(0, payload), "consume/s3-from-gcp": at(0, payload),
		"consume/tree-gcp": at(0, benchWritten), "consume/tree-aws": at(0, benchWritten),
		"warm/warm-aws": at(0, 0), "warm/warm-gcp": at(0, 0),
	}}
	second := benchRun{steps: map[string]benchStep{
		"publish/pub-aws": at(0, payload), "publish/s3-from-aws": at(0, 0), "consume/check s3-from-aws": at(0, 0),
		"warm/warm-aws": at(0, 0), "warm/warm-gcp": at(0, 0),
	}}

	return first, second
}

func TestBenchJudgmentPassesWhatMeetsEveryBudget(t *testing.T) {
	first, second := benchPassing(64 << 20)

	for _, row := range judgeBench(64<<20, first, second) {
		if !row.passed() {
			t.Errorf("hop %s %s failed inside its budget: %v", row.hop, row.route, row.problems)
		}
	}
}

// TestBenchJudgmentCatchesEachBudgetBroken is the bench's sabotage, done on the judgment: each regression the issue names, as the rows it would leave, must fail its hop.
func TestBenchJudgmentCatchesEachBudgetBroken(t *testing.T) {
	const payload = 64 << 20

	for name, c := range map[string]struct {
		hop      string
		sabotage func(first, second *benchRun)
	}{
		"a copy where an overlay belonged": {"1", func(f, _ *benchRun) { benchSet(f, "relay/same-aws", 0, payload+benchWritten) }},
		"a disk nobody weighed":            {"1", func(f, _ *benchRun) { f.steps["relay/same-aws"] = benchStep{} }},
		"a relay that crossed twice":       {"2", func(f, _ *benchRun) { benchSet(f, "relay/cross-gcp", 4*payload, payload+benchWritten) }},
		"a put that ran here":              {"3", func(f, _ *benchRun) { delete(f.steps, "publish/s3-from-aws") }},
		"a put that moved the tree":        {"3", func(f, _ *benchRun) { benchSet(f, "publish/s3-from-aws", payload, 0) }},
		"a get that kept nothing":          {"3", func(f, _ *benchRun) { benchSet(f, "consume/s3-from-aws", 0, 0) }},
		"a rerun that re-ran":              {"4", func(_, s *benchRun) { benchSet(s, "relay/cross-gcp", 0, 0) }},
		"a rerun that moved bytes":         {"4", func(_, s *benchRun) { benchSet(s, "publish/s3-from-aws", payload, 0) }},
		"a cache that left its worker":     {"5", func(_, s *benchRun) { benchSet(s, "warm/warm-aws", payload, 0) }},
		"a run that failed":                {"-", func(f, _ *benchRun) { f.err = errors.New("stale tree in payload") }},
	} {
		t.Run(name, func(t *testing.T) {
			first, second := benchPassing(payload)
			c.sabotage(&first, &second)

			for _, row := range judgeBench(payload, first, second) {
				if row.hop == c.hop && !row.passed() {
					return
				}
			}

			t.Errorf("no hop %s row failed", c.hop)
		})
	}
}

func benchSet(run *benchRun, key string, sent, disk int64) {
	run.steps[key] = benchStep{placement: store.Placement{BytesSent: sent, VolumeBytes: &disk}}
}

// TestBenchVerifyRefusesAStaleTree: the content half of every hop is the consumer's own check, so it has to fail on a tree from another run, and on one whose bytes are not the ones digested.
func TestBenchVerifyRefusesAStaleTree(t *testing.T) {
	requireDockerE2E(t)

	for name, c := range map[string]struct{ tamper, want string }{
		"another run's nonce": {`printf other > payload/nonce.txt`, "stale tree"},
		"bytes not digested":  {`printf x >> payload/blob.bin`, "does not match its digest"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writePipeline(t, t.TempDir(), `
jobs:
- name: verify
  plan:
  - task: seed
    image: `+benchImage+`
    outputs: [payload]
    run: |
`+indent(benchSeed("payload", 4096, "fresh")+"\n"+c.tamper, "      ")+`
  - task: check
    image: `+benchImage+`
    inputs: [payload]
    outputs: [out]
    run: |
`+indent(benchVerify("payload", "out", "fresh"), "      ")+`
`)

			var err error

			logs := captureStderr(t)
			out := captureStdout(t, func() { err = cli.Run([]string{path}) }) + logs()
			if err == nil || !strings.Contains(out, c.want) {
				t.Errorf("verify passed or said something else (%v):\n%s", err, out)
			}
		})
	}
}
