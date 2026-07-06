package main

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
	"github.com/jtarchie/steps/toolreg"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

const echoInputWF = `
const work = { write: "out/x.txt", content: ({ msg }) => msg };
export default {
  name: "manual",
  model: "mock",
  description: "writes msg to a file",
  input: { msg: { type: "string", required: true }, note: "string" },
  states: { work },
};`

// newTriggerServer returns the wired echo plus the store, for full HTTP tests.
func newTriggerServer(t *testing.T, wf string, maxInFlight int) (http.Handler, journal.Store) {
	t.Helper()
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})
	reg := &served{
		byName: map[string]*servedMachine{m.Name: {m: m, inputs: map[string]any{}}},
		byPath: map[string]*servedMachine{},
	}
	return newServer(t.Context(), store, eng, reg, maxInFlight), store
}

// postMultipart builds a multipart request: fields as text parts, files as
// uploads keyed "file_<name>" with the given content.
func postMultipart(t *testing.T, target string, fields, files map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		err := w.WriteField(k, v)
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		fw, err := w.CreateFormFile("file_"+name, name+".txt")
		if err != nil {
			t.Fatal(err)
		}
		_, err = fw.Write([]byte(content))
		if err != nil {
			t.Fatal(err)
		}
	}
	err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestMachinesIndexAndFormRenderInputs(t *testing.T) {
	e, _ := newTriggerServer(t, echoInputWF, 0)

	// Index lists the machine.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/machines", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /machines = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "manual") || !strings.Contains(body, "writes msg to a file") {
		t.Errorf("/machines body missing machine name/description: %q", body)
	}

	// Form renders a text + file field per declared input, plus the JSON extras.
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/machines/manual", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /machines/manual = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="in_msg"`, `name="file_msg"`,
		`name="in_note"`, `name="file_note"`,
		`name="extras_json"`,
		"Run",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("trigger form missing %q", want)
		}
	}
}

func TestTriggerEnqueuesAndDispatcherRuns(t *testing.T) {
	t.Chdir(t.TempDir())
	// mock model so the write state can render; maxInFlight 1 -> dispatcher runs it.
	e, store := newTriggerServer(t, echoInputWF, 1)

	req := postMultipart(t, "/machines/manual/run", map[string]string{"in_msg": "hello world"}, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST trigger = %d body=%s, want 303", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/runs/") {
		t.Fatalf("redirect = %q, want /runs/<id>", loc)
	}

	id := waitForSingleRun(t, store, journal.StatusDone)
	events, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if in := runInputs(events); in["msg"] != "hello world" {
		t.Errorf("enqueued inputs = %#v, want msg=hello world", in)
	}
}

func TestTriggerFileUploadBecomesStringInput(t *testing.T) {
	e, store := newTriggerServer(t, echoInputWF, 0) // no dispatcher: assert at enqueue

	// Both a text value and a file for msg — the file must win.
	req := postMultipart(t, "/machines/manual/run",
		map[string]string{"in_msg": "typed"},
		map[string]string{"msg": "from-file-content"})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST trigger = %d body=%s, want 303", rec.Code, rec.Body.String())
	}

	id := waitForSingleRun(t, store, journal.StatusQueued)
	events, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if in := runInputs(events); in["msg"] != "from-file-content" {
		t.Errorf("input msg = %#v, want the uploaded file content (file beats text)", in["msg"])
	}
}

func TestTriggerMissingRequired(t *testing.T) {
	e, _ := newTriggerServer(t, echoInputWF, 0)

	// Provide only the optional note, not the required msg.
	req := postMultipart(t, "/machines/manual/run", map[string]string{"in_note": "x"}, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing required = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "msg") || !strings.Contains(body, "missing required") {
		t.Errorf("400 body should name the missing input and re-render the form: %q", body)
	}
	// Re-render preserves the typed note.
	if !strings.Contains(body, ">x</textarea>") {
		t.Errorf("re-render should preserve entered values, got %q", body)
	}
}

func TestTriggerExtrasJSONPrecedence(t *testing.T) {
	e, store := newTriggerServer(t, echoInputWF, 0)

	// Extras supplies an undeclared key (passes through) and the declared msg,
	// which the form field must override.
	req := postMultipart(t, "/machines/manual/run", map[string]string{
		"in_msg":      "from-field",
		"extras_json": `{"msg": "from-extras", "extra": "kept"}`,
	}, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST trigger = %d body=%s, want 303", rec.Code, rec.Body.String())
	}

	id := waitForSingleRun(t, store, journal.StatusQueued)
	events, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	in := runInputs(events)
	if in["msg"] != "from-field" {
		t.Errorf("msg = %#v, want the form field to override extras", in["msg"])
	}
	if in["extra"] != "kept" {
		t.Errorf("undeclared extras key dropped: %#v", in)
	}
}

func TestTriggerMalformedExtrasJSON(t *testing.T) {
	e, _ := newTriggerServer(t, echoInputWF, 0)

	req := postMultipart(t, "/machines/manual/run", map[string]string{
		"in_msg":      "ok",
		"extras_json": `{not json`,
	}, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed extras = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "JSON object") {
		t.Errorf("400 body should explain the JSON requirement: %q", rec.Body.String())
	}
}

func TestTriggerUnknownMachine404(t *testing.T) {
	e, _ := newTriggerServer(t, echoInputWF, 0)

	for _, target := range []string{"/machines/nope", "/machines/nope/run"} {
		method := http.MethodGet
		if strings.HasSuffix(target, "/run") {
			method = http.MethodPost
		}
		req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", method, target, rec.Code)
		}
	}
}

// A machine name that isn't URL-safe must round-trip through PathEscape in the
// link and PathUnescape in the handler.
func TestTriggerEscapedMachineName(t *testing.T) {
	wf := `
const work = { write: "out/x.txt", content: ({ msg }) => msg };
export default {
  name: "my machine",
  model: "mock",
  input: { msg: { type: "string", required: true } },
  states: { work },
};`
	e, store := newTriggerServer(t, wf, 0)

	// The index links to the escaped name.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/machines", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "/machines/my%20machine") {
		t.Fatalf("index should link to the escaped name: %q", rec.Body.String())
	}

	// GET and POST both resolve the escaped param.
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/machines/my%20machine", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET escaped name = %d, want 200", rec.Code)
	}

	req = postMultipart(t, "/machines/my%20machine/run", map[string]string{"in_msg": "hi"}, nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST escaped name = %d, want 303", rec.Code)
	}
	waitForSingleRun(t, store, journal.StatusQueued)
}

func TestTriggerNoInputBlockJSONOnly(t *testing.T) {
	wf := `
const work = { write: "out/x.txt", content: ({ msg }) => msg || "none" };
export default {
  name: "freeform",
  model: "mock",
  states: { work },
};`
	e, store := newTriggerServer(t, wf, 0)

	// No declared inputs — everything comes from the JSON textarea.
	req := postMultipart(t, "/machines/freeform/run", map[string]string{
		"extras_json": `{"msg": "hi", "count": 3}`,
	}, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST trigger = %d body=%s, want 303", rec.Code, rec.Body.String())
	}

	id := waitForSingleRun(t, store, journal.StatusQueued)
	events, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	in := runInputs(events)
	if in["msg"] != "hi" {
		t.Errorf("msg = %#v, want hi from JSON", in["msg"])
	}
}

func TestTriggerQueueFull429(t *testing.T) {
	wf := `
const work = { write: "out/x.txt", content: ({ msg }) => msg };
export default {
  name: "capped",
  model: "mock",
  input: { msg: { type: "string", required: true } },
  states: { work },
  webhook: { path: "cap", map: ({ body }) => ({ msg: body.v }), maxQueued: 1 },
};`
	// No dispatcher (maxInFlight 0) so the queue never drains.
	e, _ := newTriggerServer(t, wf, 0)

	post := func() int {
		req := postMultipart(t, "/machines/capped/run", map[string]string{"in_msg": "x"}, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(); code != http.StatusSeeOther { // queue -> 1 (== maxQueued)
		t.Fatalf("post 1 = %d, want 303", code)
	}
	if code := post(); code != http.StatusTooManyRequests { // full -> 429
		t.Fatalf("post 2 = %d, want 429", code)
	}
}

func TestTriggerBodyTooLarge(t *testing.T) {
	e, _ := newTriggerServer(t, echoInputWF, 0)

	orig := maxTriggerBody
	maxTriggerBody = 64 // bytes
	t.Cleanup(func() { maxTriggerBody = orig })

	req := postMultipart(t, "/machines/manual/run", nil,
		map[string]string{"msg": strings.Repeat("A", 4096)})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d, want 413", rec.Code)
	}
}

func TestLoadServedRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	wf := `export default { name: "dup", model: "mock", states: { a: { write: "o", content: () => "x" } } };`
	hookWF := `export default { name: "dup", model: "mock", states: { a: { write: "o", content: () => "x" } }, webhook: { path: "p", map: () => ({}) } };`
	hookPath := filepath.Join(dir, "hook.ts")
	machPath := filepath.Join(dir, "mach.ts")
	writeFile(t, hookPath, hookWF)
	writeFile(t, machPath, wf)

	_, err := loadServed([]string{hookPath}, []string{machPath}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate machine name") {
		t.Fatalf("err = %v, want a duplicate-name rejection", err)
	}
}

func TestLoadServedMachineWithoutWebhook(t *testing.T) {
	dir := t.TempDir()
	wf := `export default { name: "plain", model: "mock", states: { a: { write: "o", content: () => "x" } } };`
	p := filepath.Join(dir, "plain.ts")
	writeFile(t, p, wf)

	reg, err := loadServed(nil, []string{p}, nil, nil)
	if err != nil {
		t.Fatalf("loadServed = %v, want nil (no webhook required for --machine)", err)
	}
	if _, ok := reg.byName["plain"]; !ok {
		t.Errorf("machine not registered by name: %#v", reg.byName)
	}
	if len(reg.byPath) != 0 {
		t.Errorf("byPath should be empty for a --machine with no webhook: %#v", reg.byPath)
	}
}
