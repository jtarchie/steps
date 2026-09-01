package venue

// gceClient below the gceAPI seam, against an httptest compute service: the
// fake in gcp_test.go proves the venue's wiring, and these prove the adapter
// itself reads Compute Engine's answers — operation waits above all, whose
// misreading is a silent ten-minute timeout rather than a red error.

import (
	"context"
	"encoding/json"
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
