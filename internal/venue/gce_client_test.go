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
	"strings"
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
