package venue

// Spot and preemption notices for a docker+ worker steps provisioned, read by a poll loop on the machine itself over one long-lived ssh exec: no image, and no container behind IMDSv2's hop limit of one, which drops a token PUT made from a bridge network.

import (
	"strconv"
	"strings"
)

//nolint:gochecknoglobals // test seams: the metadata services, and how often they are asked
var (
	awsMetadataBase = "http://169.254.169.254"
	gcpMetadataBase = "http://metadata.google.internal"
	drainPoll       = 5
	// cloudDockerSocket is where steps' own provisioning leaves dockerd's socket on aws:// and gcp:// machines.
	cloudDockerSocket = defaultDockerSocket
)

// untilHangup runs loop until it prints a notice and exits, or until the session's stdin closes: sshd signals nothing to a non-pty command when its client goes, so without the reader the loop would outlive the session on the machine until reboot.
func untilHangup(loop string) string {
	// fd 3, because a non-interactive shell hands every background job /dev/null for stdin: the reader would see EOF at once and stop the loop before it asked anything.
	return `exec 3<&0; (` + loop + `) & p=$!; (cat <&3 >/dev/null; kill $p 2>/dev/null) >/dev/null 2>&1 & wait $p`
}

// awsDrainScript prints a spot interruption notice and exits once EC2 posts one; a rebalance recommendation is advisory and not a reclamation.
func awsDrainScript() string {
	base := shellQuote(awsMetadataBase)

	return untilHangup(`while :; do ` +
		`t=$(curl -sf -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 300' ` + base + `/latest/api/token) || t=; ` +
		`if [ -n "$t" ]; then n=$(curl -sf -m 2 -H "X-aws-ec2-metadata-token: $t" ` + base + `/latest/meta-data/spot/instance-action); else n=$(curl -sf -m 2 ` + base + `/latest/meta-data/spot/instance-action); fi && { echo "spot $n"; exit 0; }; ` +
		`sleep ` + strconv.Itoa(drainPoll) + `; done`)
}

// gcpDrainScript polls the preempted flag rather than long-polling it: wait_for_change without the last etag misses a flip between two requests until the next timeout, longer than GCE's default thirty-second notice.
func gcpDrainScript() string {
	return untilHangup(`while :; do ` +
		`p=$(curl -sf -m 2 -H 'Metadata-Flavor: Google' ` + shellQuote(gcpMetadataBase+"/computeMetadata/v1/instance/preempted") + `) && [ "$p" = TRUE ] && { echo preempted; exit 0; }; ` +
		`sleep ` + strconv.Itoa(drainPoll) + `; done`)
}

// watchDrain runs the drain script for the session's life; closing the ssh client ends it.
func (s *plusSession) watchDrain() {
	if s.drainScript == "" {
		return
	}

	session, err := s.ssh.NewSession()
	if err != nil {
		return
	}

	// Held open and never written: its EOF, when the client goes, is the loop's signal to stop.
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()

		return
	}

	go func() {
		defer func() { _ = stdin.Close() }()
		defer func() { _ = session.Close() }()

		out, runErr := session.Output(s.drainScript)
		if runErr != nil {
			return
		}

		if reason := strings.TrimSpace(string(out)); reason != "" {
			s.drain.Store(&reason)
		}
	}()
}
