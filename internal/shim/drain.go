package shim

// Watching for the machine's own end.
//
// A spot instance is told it is going away about two minutes ahead, through
// the instance metadata service and nowhere else — the orchestrator has no
// way to learn it except from the machine itself. So the shim polls, and
// relays what it sees as a draining frame, which is the one thing this end
// ever says without being asked.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// imdsBase is the link-local address every EC2 instance answers metadata on.
// Unreachable anywhere else, which is what makes the probe self-limiting: a
// worker that is not an EC2 instance fails the first call and stops.
const imdsBase = "http://169.254.169.254"

// drainPoll is how often the metadata service is asked. A spot notice gives
// roughly two minutes, so five seconds spends a negligible fraction of the
// warning to learn it promptly.
const drainPoll = 5 * time.Second

// imdsTimeout bounds one metadata call. The service is link-local and answers
// in single-digit milliseconds; anything slower is a machine that has other
// problems.
const imdsTimeout = 2 * time.Second

// spotAction is what the metadata service reports when an instance is being
// reclaimed.
type spotAction struct {
	Action string `json:"action"`
	Time   string `json:"time"`
}

// watchForDrain polls the metadata service until the session ends, sending at
// most one draining frame.
//
// At most one deliberately: the notice does not change once given, and a
// stream of them would say nothing new while stealing the writer lock from a
// command's output.
func (s *session) watchForDrain(ctx context.Context) {
	defer s.drains.Done()

	client := &http.Client{Timeout: imdsTimeout}

	ticker := time.NewTicker(drainPoll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
		}

		token := imdsToken(ctx, client)

		notice, ok := spotNotice(ctx, client, token)
		if !ok {
			continue
		}

		// Best effort: a session already gone cannot be told, and the
		// orchestrator learns the same thing from the connection dying.
		_ = s.send(wire.FrameDraining, wire.DrainOp, notice)

		return
	}
}

// imdsToken fetches an IMDSv2 session token. An instance configured to
// require IMDSv2 — the default for new AMIs, and enforced in most accounts —
// answers nothing without one; an instance allowing v1 ignores the header.
func imdsToken(ctx context.Context, client *http.Client) string {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, imdsBase+"/latest/api/token", nil)
	if err != nil {
		return ""
	}

	request.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")

	response, err := client.Do(request)
	if err != nil {
		return ""
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return ""
	}

	token := make([]byte, 128)

	n, _ := response.Body.Read(token)

	return strings.TrimSpace(string(token[:n]))
}

// spotNotice asks whether this instance is being reclaimed, and reports the
// notice when it is.
//
// Two questions, in the order that matters: an eviction is definite and a
// rebalance recommendation is a warning that one is likely. Both are worth
// relaying, because both mean the orchestrator should stop trusting this
// machine with new work.
func spotNotice(ctx context.Context, client *http.Client, token string) (wire.Draining, bool) {
	action, ok := imdsGet(ctx, client, token, "/latest/meta-data/spot/instance-action")
	if ok {
		notice := spotAction{}

		err := json.Unmarshal([]byte(action), &notice)
		if err == nil && notice.Action != "" {
			return wire.Draining{
				Reason:   "EC2 spot " + notice.Action,
				Deadline: notice.Time,
			}, true
		}

		return wire.Draining{Reason: "EC2 spot instance-action: " + action}, true
	}

	rebalance, ok := imdsGet(ctx, client, token, "/latest/meta-data/events/recommendations/rebalance")
	if ok {
		return wire.Draining{Reason: "EC2 rebalance recommendation: " + rebalance}, true
	}

	return wire.Draining{}, false
}

// imdsGet reads one metadata path. A 404 is the ordinary answer — it is how
// the service says "no notice" — so absence is reported as not-ok rather than
// as an error.
func imdsGet(ctx context.Context, client *http.Client, token, path string) (string, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsBase+path, nil)
	if err != nil {
		return "", false
	}

	if token != "" {
		request.Header.Set("X-aws-ec2-metadata-token", token)
	}

	response, err := client.Do(request)
	if err != nil {
		return "", false
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return "", false
	}

	body := make([]byte, 1024)

	n, _ := response.Body.Read(body)

	return strings.TrimSpace(string(body[:n])), true
}
