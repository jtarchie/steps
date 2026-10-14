package llmclient

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

// A retried request's line fires mid-build, so it carries the run's identity
// like every other line under it (#121). Not t.Parallel(): swaps the default logger.
func TestRetryLinesCarryTheBuildIdentity(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(events.LogHandler(slog.NewTextHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := events.WithLogAttrs(t.Context(), "pipeline", "app", "job", "review")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://x", nil)
	if err != nil {
		t.Fatal(err)
	}

	(&requestRetryTransport{}).logRetry(req.Context(), &http.Response{StatusCode: 503}, nil, 1, 2)

	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "msg=agent.request_retry ") {
			if !strings.Contains(line, "pipeline=app") || !strings.Contains(line, "job=review") {
				t.Errorf("agent.request_retry lacks its identity: %q", line)
			}

			return
		}
	}

	t.Errorf("no agent.request_retry line:\n%s", buf.String())
}
