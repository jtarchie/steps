package llmclient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func capRequest() *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
}

func TestGenerateCapsNonJSONBodies(t *testing.T) {
	t.Parallel()

	chunk := bytes.Repeat([]byte("x"), 64<<10)

	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(status)

			for written := 0; written <= maxResponseBytes+len(chunk); written += len(chunk) {
				_, err := w.Write(chunk)
				if err != nil {
					return
				}
			}
		}))
		t.Cleanup(server.Close)

		m := newChatModel(server.Client(), server.URL, "", "gpt-4o")

		_, err := m.generate(context.Background(), capRequest())
		if !errors.Is(err, errResponseTooLarge) {
			t.Fatalf("status %d: want errResponseTooLarge, got %v", status, err)
		}

		if strings.Contains(err.Error(), "xxxx") {
			t.Fatalf("status %d: error echoes body: %.80s", status, err)
		}
	}
}

func TestGenerateSmallNonJSONBodyUnchanged(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	}))
	t.Cleanup(server.Close)

	m := newChatModel(server.Client(), server.URL, "", "gpt-4o")

	_, err := m.generate(context.Background(), capRequest())

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("want APIError 502, got %v", err)
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(ok.Close)

	_, err = newChatModel(ok.Client(), ok.URL, "", "gpt-4o").generate(context.Background(), capRequest())
	if err == nil || !strings.Contains(err.Error(), "decoding the chat response") || errors.Is(err, errResponseTooLarge) {
		t.Fatalf("want decode error, got %v", err)
	}
}
