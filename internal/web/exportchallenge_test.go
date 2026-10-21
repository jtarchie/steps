package web

import (
	"net/http"
	"strings"
	"testing"
)

// The export route answers before anybody is known, and its names come off the URL: they must not close the quoted challenge and add parameters of the caller's choosing.
func TestTheExportChallengeEscapesTheNamesItEchoes(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)

	rec := serve(t, server, http.MethodPost, "/mcp/x/ops/support%22,%20error=%22x", "{}", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated export call = %d, want 401", rec.Code)
	}

	challenge := rec.Header().Get("WWW-Authenticate")
	if strings.Count(challenge, `"`) != 2 {
		t.Errorf("the challenge carries the caller's quote: %s", challenge)
	}
}
