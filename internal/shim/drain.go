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
	"io"
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

// maxMetadataBytes bounds what is read from the metadata service. Tokens and
// notices are a few hundred bytes; the cap is what keeps a misdirected
// endpoint from being read without limit.
const maxMetadataBytes = 4096

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

	// advised remembers that the softer notice has been sent, so it is not
	// repeated while the watcher keeps looking for a real reclamation.
	advised := false

	// A transport with no proxy, deliberately: the default one reads
	// HTTP_PROXY from the environment, and Go bypasses only loopback — not
	// link-local — so an instance with a proxy configured would send every
	// metadata probe to it and never learn it was being reclaimed.
	client := &http.Client{
		Timeout:   imdsTimeout,
		Transport: &http.Transport{Proxy: nil},
	}

	defer client.CloseIdleConnections()

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

		notice, terminal := spotNotice(ctx, client, token)
		if notice.Reason == "" || (advised && !terminal) {
			continue
		}

		// Best effort: a session already gone cannot be told, and the
		// orchestrator learns the same thing from the connection dying.
		_ = s.send(wire.FrameDraining, wire.DrainOp, notice)

		if terminal {
			// Nothing more to say: the machine is going, and repeating it
			// would steal the writer from a command's output for no news.
			return
		}

		// An advisory notice is said once and then watched past, in case the
		// reclamation it warns about actually arrives.
		advised = true
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

	// Read to completion rather than one Read into a fixed buffer: an
	// io.Reader may return the body in pieces, and a truncated token is
	// silently rejected by IMDS — leaving the watcher polling forever on
	// exactly the instances (HttpTokens=required) this path exists for.
	token, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(token))
}

// spotNotice asks whether this instance is being reclaimed, reporting the
// notice and whether it is definite.
//
// Two questions, in the order that matters: an instance-action is a decision
// already taken, and a rebalance recommendation is a warning that one is
// likelier. Both are worth relaying — both mean the orchestrator should stop
// trusting this machine with new work — but only the first is worth
// destroying a machine over.
func spotNotice(ctx context.Context, client *http.Client, token string) (wire.Draining, bool) {
	action, ok := imdsGet(ctx, client, token, "/latest/meta-data/spot/instance-action")
	if ok {
		notice := spotAction{}

		err := json.Unmarshal([]byte(action), &notice)
		if err == nil && notice.Action != "" {
			// Terminal: an instance-action is a decision AWS has already
			// taken, not a forecast.
			return wire.Draining{
				Reason:   "EC2 spot " + notice.Action,
				Deadline: notice.Time,
				Terminal: true,
			}, true
		}

		return wire.Draining{Reason: "EC2 spot instance-action: " + action, Terminal: true}, true
	}

	rebalance, ok := imdsGet(ctx, client, token, "/latest/meta-data/events/recommendations/rebalance")
	if ok {
		// NOT terminal: a rebalance recommendation is a hint that an
		// interruption is likelier, and may never be followed by one. Worth
		// saying so the orchestrator stops trusting the machine with new
		// work; not worth destroying a healthy instance over.
		return wire.Draining{Reason: "EC2 rebalance recommendation: " + rebalance}, false
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

	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes))
	if err != nil {
		return "", false
	}

	value := strings.TrimSpace(string(body))
	if value == "" {
		// A 200 with nothing in it is not a notice. Reporting one would
		// fabricate an eviction and terminate a healthy machine.
		return "", false
	}

	return value, true
}
