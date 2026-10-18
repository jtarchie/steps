package resource

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// shareKeyed are the shell.RunnerSpec fields sharedCheckFor folds into its
// key: each changes how the check runs, so two checks differing in one must
// not share an answer.
var shareKeyed = map[string]bool{
	"Image": true, "Env": true, "User": true, "Network": true,
	"Privileged": true, "CPUShares": true, "MemoryBytes": true,
}

// shareUnkeyed are the fields left out of the key on purpose. A check never
// has a tree, and everything about a worker is unreachable here: a placed
// check never shares (TestAPlacedCheckDoesNotShare), so its spec never gets
// as far as a key.
var shareUnkeyed = map[string]string{
	"Cwd":          "a check has no tree",
	"MountPath":    "a check has no tree",
	"Subdir":       "a check has no tree",
	"Fetch":        "a check has no tree",
	"FetchAll":     "a check has no tree",
	"HoldAs":       "a check has no tree",
	"DeferFetch":   "a check has no tree",
	"Keep":         "a check has no tree",
	"RemoteInputs": "a check has no tree",
	"Caches":       "a check has no tree, and resources take no caches:",
	"Volumes":      "a check has no tree",
	"DockerHost":   "set only by a venue, and a placed check never shares",
	"DockerDial":   "set only by a venue, and a placed check never shares",
	"EnvValues":    "set only by a venue, and a placed check never shares",
	"Worker":       "set only by a venue, and a placed check never shares",
	"WorkerTag":    "set only by a venue, and a placed check never shares",
	"ReusedWarm":   "set only by a venue, and a placed check never shares",
}

// A RunnerSpec field nobody classified would be missing from the share key,
// and two checks differing only in it would share one answer.
func TestShareKeyCoversRunnerSpec(t *testing.T) {
	t.Parallel()

	ctx := WithCheckShare(context.Background(), NewCheckShare(time.Hour))
	_, base := sharedCheckFor(ctx, "true", shell.RunnerSpec{})

	specType := reflect.TypeFor[shell.RunnerSpec]()
	seen := map[string]bool{}

	for field := range specType.Fields() {
		seen[field.Name] = true

		if _, ok := shareUnkeyed[field.Name]; ok {
			continue
		}

		if !shareKeyed[field.Name] {
			t.Errorf("shell.RunnerSpec.%s is neither in the check share key nor listed in shareUnkeyed with a reason", field.Name)

			continue
		}

		var spec shell.RunnerSpec

		perturb(t, reflect.ValueOf(&spec).Elem().FieldByName(field.Name))

		if _, key := sharedCheckFor(ctx, "true", spec); key == base {
			t.Errorf("shell.RunnerSpec.%s is listed as keyed but does not move the check share key", field.Name)
		}
	}

	for name := range shareKeyed {
		if !seen[name] {
			t.Errorf("shareKeyed names %s, which shell.RunnerSpec no longer has", name)
		}
	}

	for name := range shareUnkeyed {
		if !seen[name] {
			t.Errorf("shareUnkeyed names %s, which shell.RunnerSpec no longer has", name)
		}
	}
}

// The spec a check is keyed on is runnerSpec's, so whatever it sets must be
// keyed — an unkeyed field it began setting would split nothing.
func TestRunnerSpecSetsOnlyKeyedFields(t *testing.T) {
	t.Parallel()

	rt := config.ResourceType{
		Image: "alpine", Env: []string{"A"}, User: "1000", Network: "none", Privileged: true,
		Limits: &config.ContainerLimits{CPU: 512, Memory: 1 << 20},
	}

	spec := reflect.ValueOf(runnerSpec(rt, []string{"B"}))

	for i := range spec.NumField() {
		name := spec.Type().Field(i).Name
		if !spec.Field(i).IsZero() && !shareKeyed[name] {
			t.Errorf("runnerSpec sets %s, which the check share key leaves out", name)
		}
	}
}

func perturb(t *testing.T, v reflect.Value) {
	t.Helper()

	//nolint:exhaustive // the keyed fields' kinds; any other fails below
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	default:
		t.Fatalf("no perturbation for a %s field", v.Kind())
	}
}
