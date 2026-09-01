package shim

// Watching for the machine's own end.
//
// A spot instance is told it is going away ahead of time — about two minutes
// on EC2, about thirty seconds on GCE — through the instance metadata
// service and nowhere else; the orchestrator has no way to learn it except
// from the machine itself. So the shim polls, and relays what it sees as a
// draining frame, which is the one thing this end ever says without being
// asked.
//
// Both clouds answer the same link-local address, so which one this machine
// is on is itself a question the watcher settles once: GCE identifies itself
// with a Metadata-Flavor header no other service sends, and EC2 by answering
// the IMDSv2 token handshake. A machine that answers as neither cannot have
// a notice, and the watcher stops for good.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// metadataBase is the link-local address both EC2 and GCE answer instance
// metadata on. A machine that is on neither cloud refuses or blackholes the
// very first call, and the watcher stops for good then — see watchForDrain.
// A variable rather than a constant so a test can stand a server in for a
// cloud without being on one.
//
//nolint:gochecknoglobals // a test seam for the metadata service
var metadataBase = "http://169.254.169.254"

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

// metadataCloud is which cloud's metadata service this machine answers as.
type metadataCloud int

const (
	cloudUnknown metadataCloud = iota
	cloudEC2
	cloudGCE
)

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
	// cloud starts unknown and is settled by the first probe.
	advised := false
	cloud := cloudUnknown

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

		done, sent := s.pollDrain(ctx, client, advised, &cloud)
		if done {
			return
		}

		advised = advised || sent
	}
}

// pollDrain runs one probe: done says the watcher's work is over — a machine
// that cannot have a notice, or a terminal notice already relayed — and sent
// says an advisory notice went out this round.
func (s *session) pollDrain(ctx context.Context, client *http.Client, advised bool, cloud *metadataCloud) (done, sent bool) {
	if *cloud == cloudUnknown {
		detected, settled := detectCloud(ctx, client)
		if !settled {
			// On neither cloud: the metadata service is link-local and
			// answers nothing anywhere else. One failed probe settles it for
			// the life of the session — polling a machine that cannot have a
			// notice would spend timeouts every tick for nothing.
			return true, false
		}

		*cloud = detected
	}

	notice, terminal := cloudNotice(ctx, client, *cloud)
	if notice.Reason == "" || (advised && !terminal) {
		return false, false
	}

	// Best effort: a session already gone cannot be told, and the
	// orchestrator learns the same thing from the connection dying.
	_ = s.send(wire.FrameDraining, wire.DrainOp, notice)

	// A terminal notice is the watcher's last word: the machine is going,
	// and repeating it would steal the writer from a command's output for no
	// news. An advisory one is said once and then watched past, in case the
	// reclamation it warns about actually arrives.
	return terminal, !terminal
}

// detectCloud asks which cloud's metadata service is answering, reporting
// settled=false when neither is.
//
// GCE first, because its answer is the stronger claim: the response carries
// a Metadata-Flavor header nothing else sends, while EC2 is identified only
// by the token handshake answering at all.
func detectCloud(ctx context.Context, client *http.Client) (metadataCloud, bool) {
	if gceReachable(ctx, client) {
		return cloudGCE, true
	}

	if _, reachable := imdsToken(ctx, client); reachable {
		return cloudEC2, true
	}

	return cloudUnknown, false
}

// cloudNotice asks the settled cloud whether this machine is being taken.
func cloudNotice(ctx context.Context, client *http.Client, cloud metadataCloud) (wire.Draining, bool) {
	switch cloud {
	case cloudGCE:
		return gceNotice(ctx, client)
	case cloudEC2:
		token, _ := imdsToken(ctx, client)

		return spotNotice(ctx, client, token)
	case cloudUnknown:
	}

	return wire.Draining{}, false
}

// gceReachable reports whether the GCE metadata service is answering. The
// Metadata-Flavor response header is required, not just a 200: the same
// address on EC2 answers requests too, with different content.
func gceReachable(ctx context.Context, client *http.Client) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataBase+"/computeMetadata/v1/instance/id", nil)
	if err != nil {
		return false
	}

	request.Header.Set("Metadata-Flavor", "Google")

	response, err := client.Do(request)
	if err != nil {
		return false
	}

	defer func() { _ = response.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxMetadataBytes))

	return response.StatusCode == http.StatusOK && response.Header.Get("Metadata-Flavor") == "Google"
}

// gceNotice asks whether this instance is being preempted.
//
// One question, unlike EC2's two: GCE has no rebalance-recommendation
// analog, and a maintenance event on a spot instance surfaces as this same
// flag. The answer is TRUE or FALSE from the instance's first boot, so only
// the exact affirmative is a notice.
func gceNotice(ctx context.Context, client *http.Client) (wire.Draining, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataBase+"/computeMetadata/v1/instance/preempted", nil)
	if err != nil {
		return wire.Draining{}, false
	}

	request.Header.Set("Metadata-Flavor", "Google")

	response, err := client.Do(request)
	if err != nil {
		return wire.Draining{}, false
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return wire.Draining{}, false
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes))
	if err != nil {
		return wire.Draining{}, false
	}

	if !strings.EqualFold(strings.TrimSpace(string(body)), "TRUE") {
		return wire.Draining{}, false
	}

	// Terminal: by the time the flag flips the decision is taken, and the
	// ACPI shutdown follows within about thirty seconds. No deadline field,
	// because GCE publishes no timestamp to relay.
	return wire.Draining{Reason: "GCE preemption", Terminal: true}, true
}

// imdsToken fetches an IMDSv2 session token, reporting whether the metadata
// service ANSWERED at all — a 4xx is an answer, a refused or timed-out dial
// is not, and only the second means this is not an EC2 instance. An instance
// configured to require IMDSv2 — the default for new AMIs — returns nothing
// useful without the token; an instance allowing v1 ignores the header.
func imdsToken(ctx context.Context, client *http.Client) (string, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, metadataBase+"/latest/api/token", nil)
	if err != nil {
		return "", false
	}

	request.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")

	response, err := client.Do(request)
	if err != nil {
		return "", false
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return "", true
	}

	// Read to completion rather than one Read into a fixed buffer: an
	// io.Reader may return the body in pieces, and a truncated token is
	// silently rejected by IMDS — leaving the watcher polling forever on
	// exactly the instances (HttpTokens=required) this path exists for.
	token, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes))
	if err != nil {
		return "", true
	}

	return strings.TrimSpace(string(token)), true
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataBase+path, nil)
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
