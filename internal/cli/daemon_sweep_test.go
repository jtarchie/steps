package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A daemon killed with a machine running restarts and gives it back (#120), through the identity in its database, the worker mapping, the real SDK and EC2's wire format; not t.Parallel() because the SDK reads its endpoint and credentials from the environment.
func TestARestartedDaemonParksTheMachineItLeftRunning(t *testing.T) {
	state := State(filepath.Join(t.TempDir(), "steps.db"))

	owner, err := sqlite.Identity(t.Context(), string(state))
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}

	now := time.Now().UTC()
	ec2 := &recordingEC2{instances: []describedInstance{
		{id: "i-0abc123def456789", stamp: fmt.Sprintf("%s-stopped-%d", owner, now.Unix()), launched: now},
		{id: "i-0fed987cba654321", stamp: fmt.Sprintf("0123456789abcdef-stopped-%d", now.Unix()), launched: now},
	}}

	server := httptest.NewServer(ec2)
	t.Cleanup(server.Close)

	t.Setenv("AWS_ENDPOINT_URL_EC2", server.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))

	ctx, err := pipeline.WithWorkers(t.Context(), map[string]string{"box": "aws://stopped/i-0abc123def456789"})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	held := newDaemon(ctx, nil, scheduler.NewLocalRunner(nil, pipeline.Run{}, 1), state, ExecFlags{}, HistoryFlags{}, time.Hour)
	defer held.Close()

	err = held.load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	want := []string{"DeleteTags i-0abc123def456789", "StopInstances i-0abc123def456789"}
	if got := ec2.mutations(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v: untagged before stopped, and nothing done to another daemon's machine", got, want)
	}
}

type describedInstance struct {
	id, stamp string
	launched  time.Time
}

// recordingEC2 answers DescribeInstances with running instances and records every other action, in EC2's query protocol.
type recordingEC2 struct {
	instances []describedInstance

	mu    sync.Mutex
	calls []string
}

func (e *recordingEC2) mutations() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return slices.Clone(e.calls)
}

func (e *recordingEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	action := r.Form.Get("Action")

	if action != "DescribeInstances" {
		e.mu.Lock()
		e.calls = append(e.calls, action+" "+r.Form.Get("InstanceId.1")+r.Form.Get("ResourceId.1"))
		e.mu.Unlock()

		// Every action the sweep may send acknowledges the same way; the SDK reads nothing from these answers.
		_, _ = io.WriteString(w, "<Response><requestId>r</requestId><return>true</return></Response>")

		return
	}

	var body strings.Builder

	body.WriteString("<DescribeInstancesResponse><requestId>r</requestId><reservationSet><item><instancesSet>")

	for _, instance := range e.instances {
		body.WriteString("<item><instanceId>" + instance.id + "</instanceId>" +
			"<instanceState><code>16</code><name>running</name></instanceState>" +
			"<launchTime>" + instance.launched.Format(time.RFC3339) + "</launchTime>" +
			"<tagSet><item><key>steps-owner</key><value>" + instance.stamp + "</value></item></tagSet></item>")
	}

	body.WriteString("</instancesSet></item></reservationSet></DescribeInstancesResponse>")

	_, _ = io.WriteString(w, body.String())
}
