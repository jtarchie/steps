package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func postBody(t *testing.T, server *Server, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

// Form parsing would buffer 10MB of a POST nobody has signed in for; the page routes get the small default and the routes with their own limits keep them.
func TestRequestBodiesAreBounded(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	read := make(chan int, 1)
	pipeline.Hooks = func(w http.ResponseWriter, r *http.Request, _ string) {
		n, _ := io.Copy(io.Discard, r.Body)
		read <- int(n)

		w.WriteHeader(http.StatusAccepted)
	}

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	big := strings.Repeat("x", int(maxBodySize)+1)

	if rec := postBody(t, server, "/oauth/token", "code="+big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized page-route POST = %d, want 413", rec.Code)
	}

	if rec := postBody(t, server, "/p/demo/hooks/anything", big); rec.Code != http.StatusAccepted || <-read != len(big) {
		t.Errorf("a webhook delivery over the page limit = %d, want it left to the hook's own limit", rec.Code)
	}
}

// reason and answer are stored whole, so a megabyte of either would be a megabyte in the state database.
func TestFreeTextIsCapped(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id, err := pipeline.Store.RequestApproval(context.Background(), "build", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	long := strings.Repeat("x", maxFreeText+1)

	rec := postBody(t, server, "/p/demo/approvals/"+strconv.FormatInt(id, 10), "decision=approve&reason="+long)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized reason = %d, want 413", rec.Code)
	}

	if decided, _ := pipeline.Store.ApprovalStatus(context.Background(), id); decided.Status != "pending" {
		t.Errorf("the refused decision was recorded: %q", decided.Status)
	}

	question := askOne(t, pipeline)

	rec = postBody(t, server, "/p/demo/questions/"+strconv.FormatInt(question.ID, 10), "answer_text="+long)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized answer = %d, want 413", rec.Code)
	}
}

func TestASetLargerThanAPageBodyStillLands(t *testing.T) {
	t.Parallel()

	server, manager := managedServer(t)

	body := `{"source":"` + strings.Repeat("j", int(maxBodySize)*2) + `"}`

	if code, resp := call(t, server, http.MethodPut, "/api/pipelines/big", body); code >= http.StatusBadRequest {
		t.Errorf("a %d byte set = %d: %s", len(body), code, resp)
	}

	if len(manager.set) != 1 {
		t.Errorf("sets = %v, want the upload reaching the manager", manager.set)
	}
}
