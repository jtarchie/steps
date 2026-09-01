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
// with a Metadata-Flavor header no other service sends, and EC2 by the
// IMDSv2 token handshake granting a token. A machine where nothing answers
// at all cannot have a notice, and the watcher stops for good; one that
// answered without identifying itself is asked again next tick, because the
// likeliest such machine is a cloud machine mid-blip — and settling on a
// guess would disarm the watch for the life of the session.

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
	if done := settleCloud(ctx, client, cloud); done {
		return true, false
	}

	if *cloud == cloudUnknown {
		// Answered, but as neither cloud — a blip, most likely. Detection
		// runs again next tick; the address is alive, so the retry is cheap.
		return false, false
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

// settleCloud runs detection once if it is still needed. done says the
// watcher can never have a notice: nothing answers the link-local address at
// all, which no retry will change — the metadata service answers nothing
// anywhere outside a cloud, and polling a machine that cannot have a notice
// would spend timeouts every tick for nothing. An ANSWER that identifies
// neither cloud deliberately does not settle anything: a GCE machine whose
// identifying probe blipped still answers the EC2 token PUT (with a 404),
// and settling on that answer would poll spot paths GCE 404s forever — a
// real preemption never relayed, on the machine the watch exists for.
func settleCloud(ctx context.Context, client *http.Client, cloud *metadataCloud) (done bool) {
	if *cloud != cloudUnknown {
		return false
	}

	detected, answered := detectCloud(ctx, client)
	if detected == cloudUnknown {
		return !answered
	}

	*cloud = detected

	return false
}

// detectCloud asks which cloud's metadata service is answering. answered
// reports whether anything answered at all — a settled cloud always did,
// while cloudUnknown with answered=true is a service that spoke without
// identifying itself.
//
// GCE first, because its answer is the stronger claim: the response carries
// a Metadata-Flavor header nothing else sends. EC2 is claimed by the token
// handshake GRANTING a token — merely answering is not enough, since GCE
// answers the same PUT too, with a 404 — or, for an instance whose token PUT
// is refused while v1 GETs still work, by the /latest/ tree answering at all,
// which GCE 404s. Both are positive claims; neither settles on a bare answer,
// because doing so would disarm the watch on a GCE machine mid-blip.
func detectCloud(ctx context.Context, client *http.Client) (metadataCloud, bool) {
	if gceReachable(ctx, client) {
		return cloudGCE, true
	}

	token, reachable := imdsToken(ctx, client)
	if !reachable {
		return cloudUnknown, false
	}

	if token != "" || ec2Reachable(ctx, client, token) {
		return cloudEC2, true
	}

	return cloudUnknown, true
}

// ec2Reachable reports whether the IMDS /latest/ tree answers, which is the
// EC2 claim left when the v2 token handshake grants nothing. GCE serves no
// path under /latest/, so an answer here is as identifying as a token.
func ec2Reachable(ctx context.Context, client *http.Client, token string) bool {
	_, ok := imdsGet(ctx, client, token, "/latest/meta-data/instance-id")

	return ok
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
	_, header, ok := metadataGet(ctx, client, "/computeMetadata/v1/instance/id", "Metadata-Flavor", "Google")

	return ok && header.Get("Metadata-Flavor") == "Google"
}

// gceNotice asks whether this instance is being taken.
//
// Two keys, because GCE says it two ways and neither covers the other —
// MEASURED against the real service, not read from its docs: a market
// preemption flips `preempted` to TRUE, while a maintenance event that will
// terminate the machine (including instances.simulateMaintenanceEvent on a
// spot instance, which is Google's own preemption drill) announces itself
// only through `maintenance-event` and never touches `preempted`. A watcher
// reading one key relayed nothing for the other, and the machine's death was
// classified as a lost connection instead of an eviction.
//
// A MIGRATE maintenance event is deliberately not a notice: a live migration
// pauses the machine briefly and it survives, and fabricating an eviction
// from one would destroy a healthy worker.
func gceNotice(ctx context.Context, client *http.Client) (wire.Draining, bool) {
	preempted, ok := gceValue(ctx, client, "/computeMetadata/v1/instance/preempted")
	if ok && strings.EqualFold(preempted, "TRUE") {
		// Terminal: by the time the flag flips the decision is taken, and
		// the ACPI shutdown follows within about thirty seconds. No deadline
		// field, because GCE publishes no timestamp to relay.
		return wire.Draining{Reason: "GCE preemption", Terminal: true}, true
	}

	event, ok := gceValue(ctx, client, "/computeMetadata/v1/instance/maintenance-event")
	if ok && strings.EqualFold(event, "TERMINATE_ON_HOST_MAINTENANCE") {
		return wire.Draining{Reason: "GCE maintenance: the host is being taken out of service", Terminal: true}, true
	}

	return wire.Draining{}, false
}

// gceValue reads one GCE metadata path. Absence, emptiness and errors all
// report not-ok — the same no-fabricated-notices rule imdsGet holds.
func gceValue(ctx context.Context, client *http.Client, path string) (string, bool) {
	value, _, ok := metadataGet(ctx, client, path, "Metadata-Flavor", "Google")

	return value, ok
}

// metadataGet reads one metadata path, for either cloud — the same request,
// bound, and no-fabricated-notices rule either way; only the identifying
// header differs, and an empty header value sends none at all. Absence, a
// non-200, an empty body and errors all report not-ok: a fabricated notice
// terminates a healthy machine.
//
// The response headers come back because identifying GCE needs one: a 200 is
// not the claim, the Metadata-Flavor it answers with is.
func metadataGet(ctx context.Context, client *http.Client, path, header, headerValue string) (string, http.Header, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataBase+path, nil)
	if err != nil {
		return "", nil, false
	}

	if headerValue != "" {
		request.Header.Set(header, headerValue)
	}

	response, err := client.Do(request)
	if err != nil {
		return "", nil, false
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return "", response.Header, false
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes))
	if err != nil {
		return "", response.Header, false
	}

	value := strings.TrimSpace(string(body))
	if value == "" {
		return "", response.Header, false
	}

	return value, response.Header, true
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
// as an error. An empty token sends no header, for the v1-allowed instance
// whose token PUT is refused — the case detectCloud identifies through the
// /latest/ tree instead.
func imdsGet(ctx context.Context, client *http.Client, token, path string) (string, bool) {
	value, _, ok := metadataGet(ctx, client, path, "X-aws-ec2-metadata-token", token)

	return value, ok
}
