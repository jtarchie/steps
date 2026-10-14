package venue

// gceClient below the gceAPI seam, against an httptest compute service: the
// fake in gcp_test.go proves the venue's wiring, and these prove the adapter
// itself reads Compute Engine's answers — operation waits above all, whose
// misreading is a silent ten-minute timeout rather than a red error.

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

// newTestGCEClient serves the compute API from a handler and returns the
// adapter under test pointed at it.
func newTestGCEClient(t *testing.T, handler http.HandlerFunc) *gceClient {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	service, err := compute.NewService(t.Context(),
		option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("building the compute client: %v", err)
	}

	return &gceClient{service: service}
}

// operationJSON writes one compute.Operation as the API would.
func operationJSON(t *testing.T, w http.ResponseWriter, op compute.Operation) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")

	err := json.NewEncoder(w).Encode(op)
	if err != nil {
		t.Errorf("encoding the operation: %v", err)
	}
}

// TestGCEClientStartSurfacesTheOperationError pins that Start waits its
// operation out: GCE reports a start that cannot happen — an exhausted zone,
// a fingerprint conflict — in the operation, not the accepting call, and
// skipping the wait turns each into a silent ten-minute poll of an instance
// that stays parked.
func TestGCEClientStartSurfacesTheOperationError(t *testing.T) {
	t.Parallel()

	client := newTestGCEClient(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/start"):
			operationJSON(t, w, compute.Operation{Name: "op-start", Status: "RUNNING"})
		case strings.HasSuffix(req.URL.Path, "/operations/op-start/wait"):
			operationJSON(t, w, compute.Operation{
				Name:   "op-start",
				Status: "DONE",
				Error: &compute.OperationError{Errors: []*compute.OperationErrorErrors{{
					Code:    "ZONE_RESOURCE_POOL_EXHAUSTED",
					Message: "the zone does not have enough resources",
				}}},
			})
		default:
			http.NotFound(w, req)
		}
	})

	err := client.Start(context.Background(), "p", "z", "worker-1")
	if err == nil || !strings.Contains(err.Error(), "ZONE_RESOURCE_POOL_EXHAUSTED") {
		t.Fatalf("Start = %v, want the operation's own failure surfaced", err)
	}
}

// TestAwaitZoneOperationReadsABareHTTPFailure pins the other failure shape:
// a DONE operation whose error is expressed only as an HTTP status on the
// operation itself, which read as success would poll for a machine that was
// never coming.
func TestAwaitZoneOperationReadsABareHTTPFailure(t *testing.T) {
	t.Parallel()

	client := newTestGCEClient(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/operations/op-1/wait") {
			operationJSON(t, w, compute.Operation{
				Name:                "op-1",
				Status:              "DONE",
				HttpErrorStatusCode: http.StatusRequestEntityTooLarge,
				HttpErrorMessage:    "REQUEST ENTITY TOO LARGE",
			})

			return
		}

		http.NotFound(w, req)
	})

	err := client.awaitZoneOperation(context.Background(), "p", "z", "op-1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 413") {
		t.Fatalf("awaitZoneOperation = %v, want the bare HTTP failure read as one", err)
	}
}

// An error envelope with no entries must fall through to the status check rather than index an empty list.
func TestZoneOperationOutcomeBoundaries(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		op     compute.Operation
		failed bool
	}{
		"bare 400":                     {compute.Operation{HttpErrorStatusCode: http.StatusBadRequest}, true},
		"bare 399":                     {compute.Operation{HttpErrorStatusCode: 399}, false},
		"empty error envelope":         {compute.Operation{Error: &compute.OperationError{}}, false},
		"empty error envelope and 400": {compute.Operation{Error: &compute.OperationError{}, HttpErrorStatusCode: http.StatusBadRequest}, true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := zoneOperationOutcome(&tc.op)
			if (err != nil) != tc.failed {
				t.Errorf("zoneOperationOutcome = %v, want failed=%v", err, tc.failed)
			}
		})
	}
}

// TestAwaitZoneOperationPacesAnEagerServer pins the pause between waits: the
// SDK documents Wait as best-effort — under load it "might return after zero
// seconds" — and a loop with no pause turns that into hammering an already
// overloaded API.
func TestAwaitZoneOperationPacesAnEagerServer(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	client := newTestGCEClient(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/operations/op-1/wait") {
			calls.Add(1)
			// Returning instantly, never DONE: the overloaded-server shape.
			operationJSON(t, w, compute.Operation{Name: "op-1", Status: "PENDING"})

			return
		}

		http.NotFound(w, req)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := client.awaitZoneOperation(ctx, "p", "z", "op-1")
	if err == nil {
		t.Fatal("an operation that never finished reported success")
	}

	if calls.Load() > 2 {
		t.Errorf("an eager server was asked %d times in 300ms — the loop is not pacing itself", calls.Load())
	}
}

// TestGCEClientInsertSendsLabels pins that the labels reach the wire, not just
// the fake: the fake records whatever it is handed, and an adapter that
// dropped them would launch every machine unfindable.
func TestGCEClientInsertSendsLabels(t *testing.T) {
	t.Parallel()

	var (
		sent     compute.Instance
		template string
	)

	client := newTestGCEClient(t, fakeInsertAPI(t, &sent, &template))

	labels := map[string]string{labelWorker: "abc123", labelHost: "box", labelPid: "42"}

	err := client.InsertFromTemplate(context.Background(), "p", "z", "steps-1", "tmpl", labels)
	if err != nil {
		t.Fatalf("InsertFromTemplate: %v", err)
	}

	if template != "projects/p/global/instanceTemplates/tmpl" {
		t.Errorf("sourceInstanceTemplate = %q", template)
	}

	// The insert's labels replace the template's on GCE, so the template's are sent too, with steps' own winning.
	want := map[string]string{"team": "infra", labelWorker: "abc123", labelHost: "box", labelPid: "42"}
	if sent.Name != "steps-1" || !maps.Equal(sent.Labels, want) {
		t.Errorf("sent %s with labels %v, want steps-1 with %v", sent.Name, sent.Labels, want)
	}
}

// fakeInsertAPI answers an insert from template, its operation, and the template, which carries a label of its own and one steps sets too.
func fakeInsertAPI(t *testing.T, sent *compute.Instance, template *string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/instances"):
			*template = req.URL.Query().Get("sourceInstanceTemplate")

			err := json.NewDecoder(req.Body).Decode(sent)
			if err != nil {
				t.Errorf("decoding the insert: %v", err)
			}

			operationJSON(t, w, compute.Operation{Name: "op-insert", Status: "RUNNING"})
		case strings.HasSuffix(req.URL.Path, "/operations/op-insert/wait"):
			operationJSON(t, w, compute.Operation{Name: "op-insert", Status: "DONE"})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/global/instanceTemplates/tmpl"):
			_ = json.NewEncoder(w).Encode(compute.InstanceTemplate{Name: "tmpl", Properties: &compute.InstanceProperties{
				Labels: map[string]string{"team": "infra", labelWorker: "from-the-template"},
			}})
		default:
			http.NotFound(w, req)
		}
	}
}

// Inserting from a template needs only instanceTemplates.useReadOnly; a role that cannot read the template still launches, with steps' labels alone.
func TestGCEClientInsertLaunchesWithoutReadingTheTemplate(t *testing.T) {
	t.Parallel()

	var (
		sent     compute.Instance
		template string
	)

	inner := fakeInsertAPI(t, &sent, &template)
	client := newTestGCEClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/instanceTemplates/") {
			http.Error(w, `{"error":{"code":403,"message":"forbidden"}}`, http.StatusForbidden)

			return
		}

		inner(w, req)
	})

	labels := map[string]string{labelWorker: "abc123"}

	err := client.InsertFromTemplate(context.Background(), "p", "z", "steps-1", "tmpl", labels)
	if err != nil {
		t.Fatalf("InsertFromTemplate without template read access: %v", err)
	}

	if !maps.Equal(sent.Labels, labels) {
		t.Errorf("sent labels %v, want steps' own %v", sent.Labels, labels)
	}
}

// fakeLabelsAPI serves one instance whose labels SetLabels rewrites, refusing the first conflicts writes with 412 as a concurrent writer's fingerprint change would.
func fakeLabelsAPI(t *testing.T, labels map[string]string, conflicts int, sent *[]compute.InstancesSetLabelsRequest) http.HandlerFunc {
	t.Helper()

	var mu sync.Mutex

	fingerprint := 0

	return func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/instances/worker-1"):
			_ = json.NewEncoder(w).Encode(compute.Instance{
				Name: "worker-1", Labels: labels, LabelFingerprint: strconv.Itoa(fingerprint),
				LastStartTimestamp: "2026-10-14T05:00:00.000-07:00",
			})
		case strings.HasSuffix(req.URL.Path, "/instances/worker-1/setLabels"):
			var body compute.InstancesSetLabelsRequest

			err := json.NewDecoder(req.Body).Decode(&body)
			if err != nil {
				t.Errorf("decoding setLabels: %v", err)
			}

			*sent = append(*sent, body)

			if conflicts > 0 {
				conflicts--
				fingerprint++

				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`{"error":{"code":412,"message":"Labels fingerprint either invalid or resource labels have changed"}}`))

				return
			}

			if body.LabelFingerprint != strconv.Itoa(fingerprint) {
				t.Errorf("setLabels sent fingerprint %q, want the one just read, %d", body.LabelFingerprint, fingerprint)
			}

			clear(labels)
			maps.Copy(labels, body.Labels)

			operationJSON(t, w, compute.Operation{Name: "op-labels", Status: "RUNNING"})
		case strings.HasSuffix(req.URL.Path, "/operations/op-labels/wait"):
			operationJSON(t, w, compute.Operation{Name: "op-labels", Status: "DONE"})
		default:
			http.NotFound(w, req)
		}
	}
}

// A label write replaces every label on the instance, so it must carry the operator's own and the fingerprint just read, retrying when another writer got there first.
func TestGCEClientSetOwnerKeepsOtherLabelsAndRetriesALostRace(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"team": "infra"}

	var sent []compute.InstancesSetLabelsRequest

	client := newTestGCEClient(t, fakeLabelsAPI(t, labels, 1, &sent))

	err := client.SetOwner(context.Background(), "p", "z", "worker-1", "feedfacecafebeef-stopped-1")
	if err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	if want := map[string]string{"team": "infra", labelOwner: "feedfacecafebeef-stopped-1"}; !maps.Equal(labels, want) {
		t.Errorf("labels = %v, want %v", labels, want)
	}

	if len(sent) != 2 {
		t.Errorf("%d writes, want one refused and one retried", len(sent))
	}

	owned, err := client.Owner(context.Background(), "p", "z", "worker-1")
	if err != nil {
		t.Fatalf("Owner: %v", err)
	}

	if owned.stamp != "feedfacecafebeef-stopped-1" || !owned.started.Equal(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("Owner = %+v, want the stamp and the last start", owned)
	}

	err = client.SetOwner(context.Background(), "p", "z", "worker-1", "")
	if err != nil {
		t.Fatalf("SetOwner to remove: %v", err)
	}

	if want := map[string]string{"team": "infra"}; !maps.Equal(labels, want) {
		t.Errorf("labels after removal = %v, want %v", labels, want)
	}
}

func TestGCEClientListOwnedReadsStampedInstancesOnTheirWayUpOrRunning(t *testing.T) {
	t.Parallel()

	client := newTestGCEClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(compute.InstanceList{Items: []*compute.Instance{
			{Name: "steps-0a1b2c", Status: "STAGING", Labels: map[string]string{labelOwner: "x"}, CreationTimestamp: "2026-10-14T12:00:00Z"},
			{Name: "worker-1", Status: "RUNNING", Labels: map[string]string{labelOwner: "y"}, LastStartTimestamp: "2026-10-14T12:00:00Z"},
			{Name: "parked", Status: "TERMINATED", Labels: map[string]string{labelOwner: "z"}, LastStartTimestamp: "2026-10-14T12:00:00Z"},
			{Name: "unrelated", Status: "RUNNING", Labels: map[string]string{"team": "infra"}},
		}})
	})

	owned, err := client.ListOwned(context.Background(), "p", "z")
	if err != nil {
		t.Fatalf("ListOwned: %v", err)
	}

	if len(owned) != 2 || owned[0].name != "steps-0a1b2c" || owned[0].stamp != "x" || owned[0].started.IsZero() || owned[1].name != "worker-1" {
		t.Errorf("ListOwned = %+v, want the booting and the running stamped instances, the first started at its creation", owned)
	}
}
