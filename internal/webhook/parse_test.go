package webhook

import "testing"

// TestParseRequestKeepsTheBodyAfterTheBlankLine: a hand-written capture has no Content-Length, and the body is everything after the headers.
func TestParseRequestKeepsTheBodyAfterTheBlankLine(t *testing.T) {
	req, err := ParseRequest([]byte("POST /hooks/push?x=1 HTTP/1.1\nX-GitHub-Event: push\n\n{\"after\":\"aaa\"}\n"))
	if err != nil {
		t.Fatal(err)
	}

	if string(req.Body) != "{\"after\":\"aaa\"}\n" || req.Header.Get("X-GitHub-Event") != "push" || req.Query.Get("x") != "1" || req.Method != "POST" {
		t.Errorf("parsed %+v body=%q", req, req.Body)
	}
}
