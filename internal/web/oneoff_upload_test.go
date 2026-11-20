package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The one-off route left the /api group's body limit so a folder can be bigger than a configuration; the configuration alone is still held to it.
func TestAnOversizedOneOffIsRefusedAsOversized(t *testing.T) {
	t.Parallel()

	server, manager := managedServer(t)

	oversized := `{"job":"build","source":"` + strings.Repeat("j", int(maxUploadSize)) + `"}`

	code, body := call(t, server, http.MethodPost, oneOffPath, oversized)
	if code != http.StatusRequestEntityTooLarge || len(manager.set) != 0 {
		t.Errorf("a one-off of %d bytes answered %d and reached the manager %d times, want 413 and never: %s", len(oversized), code, len(manager.set), body)
	}
}

type uploadManager struct {
	fakeManager

	got map[string]string
	err error
}

func (u *uploadManager) OneOff(_ context.Context, req OneOffRequest) (OneOffResult, error) {
	u.got = map[string]string{}

	for {
		get, archive, err := req.Uploads.Next()
		if errors.Is(err, io.EOF) {
			return OneOffResult{Pipeline: "one-off.x", Job: req.Job}, nil
		}

		if err != nil {
			u.err = err

			return OneOffResult{}, fmt.Errorf("next upload: %w", err)
		}

		data, _ := io.ReadAll(archive)
		u.got[get] = string(data)
	}
}

func multipartOneOff(t *testing.T, parts map[string]string) (*bytes.Buffer, string) {
	t.Helper()

	var body bytes.Buffer

	form := multipart.NewWriter(&body)

	request, _ := form.CreateFormField("request")
	_, _ = request.Write([]byte(`{"source":"jobs: []","job":"build","inputs":["src"]}`))

	for name, data := range parts {
		part, _ := form.CreateFormFile(name, "x")
		_, _ = part.Write([]byte(data))
	}

	_ = form.Close()

	return &body, form.FormDataContentType()
}

// The request comes first so a refusal of it is answered before any folder is read.
func TestAOneOffWhoseFirstPartIsNotTheRequestIsRefused(t *testing.T) {
	t.Parallel()

	server, manager := managedServer(t)

	var body bytes.Buffer

	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile(uploadPart+"src", "x")
	_, _ = part.Write([]byte(`{"source":"jobs: []","job":"build"}`))
	_ = form.Close()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, oneOffPath, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || len(manager.set) != 0 {
		t.Errorf("POST %s with a folder first = %d, reaching the manager %d times; want 400 and never", oneOffPath, rec.Code, len(manager.set))
	}
}

func TestAOneOffsFoldersReachTheManagerByGet(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		part string
		code int
	}{
		"a folder":            {uploadPart + "src", http.StatusCreated},
		"a part that is none": {"other", http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server, err := New(nil, stubRunner{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			manager := &uploadManager{}
			server.SetManager(manager)

			body, contentType := multipartOneOff(t, map[string]string{tc.part: "archive-bytes"})

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, oneOffPath, body)
			req.Header.Set("Content-Type", contentType)

			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.code {
				t.Fatalf("POST %s = %d, want %d: %s", oneOffPath, rec.Code, tc.code, rec.Body)
			}

			if tc.code == http.StatusCreated && manager.got["src"] != "archive-bytes" {
				t.Errorf("the manager read %v, want src's bytes under src", manager.got)
			}
		})
	}
}
