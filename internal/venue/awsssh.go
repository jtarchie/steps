package venue

// aws:// after steps#206: the instance's docker daemon, reached by ssh over an SSM port-forward to its sshd. SSM forwards only to TCP ports, never to a unix socket, so ssh rides inside the tunnel and its streamlocal channel reaches the socket; one SendCommand installs this process's key and reports the host key to pin, which keeps IAM as the only door.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/venue/ssmdial"
)

// awsSSHUser is the account the install creates: in the docker group, holding only keys steps put there.
const awsSSHUser = "steps"

const awsHostKeyMarker = "steps-hostkey: "

var errAWSInstall = errors.New("installing steps' ssh key on the instance did not report a host key")

// awsInstalled maps an instance to the host key its install reported, so later steps skip a SendCommand that takes seconds.
var awsInstalled sync.Map //nolint:gochecknoglobals // per-process memory of what this process installed where

// awsInstalling holds a lock per instance, so concurrent dials share one install.
var awsInstalling sync.Map //nolint:gochecknoglobals // as above

func awsSSHClient(ctx context.Context, worker Worker) (*ssh.Client, error) {
	api, err := ssmAPIFor(ctx, worker)
	if err != nil {
		return nil, err
	}

	platform, err := waitForManagedNode(ctx, api, worker)
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	if platform == ssmdial.PlatformWindows {
		return nil, fmt.Errorf("%w %q: the instance runs Windows, which a docker+ worker cannot drive", ErrWorker, worker.URL)
	}

	signer, err := sshIdentity()
	if err != nil {
		return nil, err
	}

	client, err := awsConnect(ctx, api, worker, platform, signer)
	if err != nil && (isAuthRefusal(err) || strings.Contains(err.Error(), "ssh: host key mismatch")) {
		// The cached install went stale: its key expired, or the root volume was replaced under the same id (new host key, no authorized_keys). Install again, once; the new host key arrives over SSM, which IAM vouches for.
		awsInstalled.Delete(worker.Instance)

		client, err = awsConnect(ctx, api, worker, platform, signer)
	}

	return client, err
}

func awsConnect(ctx context.Context, api ssmdial.API, worker Worker, platform ssmdial.Platform, signer ssh.Signer) (*ssh.Client, error) {
	hostKey, err := awsHostKey(ctx, api, worker, platform, signer)
	if err != nil {
		return nil, err
	}

	channel, err := ssmForward(ctx, api, worker.Instance, 22)
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	client, err := sshHandshake(asConn(channel), worker.Instance+":22", &ssh.ClientConfig{
		User:            awsSSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		// Only the pinned key's type: an sshd offering several would otherwise negotiate one the pin cannot match.
		HostKeyAlgorithms: hostKeyAlgorithms(hostKey),
		Timeout:           dialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to %s for %q: %w", worker.Instance, worker.URL, err)
	}

	return client, nil
}

func isAuthRefusal(err error) bool {
	return strings.Contains(err.Error(), "ssh: unable to authenticate")
}

func awsHostKey(ctx context.Context, api ssmdial.API, worker Worker, platform ssmdial.Platform, signer ssh.Signer) (ssh.PublicKey, error) {
	// One install per instance at a time: parallel steps on a fresh machine would each send a SendCommand that waits minutes for docker.
	lock, _ := awsInstalling.LoadOrStore(worker.Instance, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()         //nolint:forcetypeassert // this map holds one type
	defer lock.(*sync.Mutex).Unlock() //nolint:forcetypeassert // as above

	if cached, ok := awsInstalled.Load(worker.Instance); ok {
		return cached.(ssh.PublicKey), nil //nolint:forcetypeassert // this map holds one type
	}

	output, err := ssmdial.Run(ctx, api, worker.Instance, platform, awsInstallScript(signer.PublicKey()))
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, awsHostKeyMarker) {
			continue
		}

		hostKey, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(strings.TrimPrefix(line, awsHostKeyMarker)))
		if parseErr != nil {
			return nil, fmt.Errorf("%w %q: %w", ErrWorker, worker.URL, parseErr)
		}

		awsInstalled.Store(worker.Instance, hostKey)

		return hostKey, nil
	}

	return nil, fmt.Errorf("%w %q: %w: %s", ErrWorker, worker.URL, errAWSInstall, strings.TrimSpace(output))
}

// awsInstallScript's key line is restrict,port-forwarding and expires in twelve hours, so an orchestrator that never comes back leaves nothing usable behind.
func awsInstallScript(key ssh.PublicKey) string {
	// ponytail: the key is never removed on release; its expiry and the next install's prune of expired lines clear it.
	marker := "steps-ephemeral-" + strings.NewReplacer("/", "_", "+", "-", ":", "-").Replace(ssh.FingerprintSHA256(key))
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " " + marker

	return `set -e
u=` + awsSSHUser + `
# A machine just launched registers with SSM before its user data has installed docker; the docker group must exist before the user is put in it.
i=0; until docker info >/dev/null 2>&1 || [ "$i" -ge 120 ]; do i=$((i + 1)); sleep 2; done
# Failed, not skipped: a user made outside the group cannot open the socket, and the cached install would never add it.
getent group docker >/dev/null 2>&1 || { echo "no docker group after four minutes: docker is not installed on this instance" >&2; exit 1; }
id "$u" >/dev/null 2>&1 || useradd -m -s /bin/sh "$u" 2>/dev/null || adduser -D -s /bin/sh "$u"
# A new account's password is "!", locked, and an sshd without PAM refuses a key for a locked account; "*" is no password and not locked.
sed -i "s/^$u:!/$u:*/" /etc/shadow 2>/dev/null || true
usermod -aG docker "$u" 2>/dev/null || addgroup "$u" docker
h=$(getent passwd "$u" | cut -d: -f6)
mkdir -p "$h/.ssh"
f="$h/.ssh/authorized_keys"
touch "$f"
now=$(date +%Y%m%d%H%M)
# Its own temp file: parallel steps on a fresh instance each send this install, and a shared one interleaves their writes.
tmp=$(mktemp "$f.XXXXXX")
# Drop this key's old line and other steps keys that have expired; a line with no expiry (a host whose date cannot add hours) is another orchestrator's live key and stays.
awk -v now="$now" -v mine=` + shellQuote(marker) + ` '
  index($0, "steps-ephemeral-") == 0 { print; next }
  index($0, mine) > 0 { next }
  !match($0, /expiry-time="[0-9]+"/) { print; next }
  substr($0, RSTART + 13, 12) > now { print }
' "$f" > "$tmp" && mv "$tmp" "$f"
if exp=$(date -d '+12 hours' +%Y%m%d%H%M 2>/dev/null); then opts="expiry-time=\"$exp\",restrict,port-forwarding"; else opts="restrict,port-forwarding"; fi
printf '%s %s\n' "$opts" ` + shellQuote(line) + ` >> "$f"
chown -R "$u" "$h/.ssh"
chmod 700 "$h/.ssh"
chmod 600 "$f"
echo "` + awsHostKeyMarker + `$(cat /etc/ssh/ssh_host_ed25519_key.pub)"
`
}

// asConn gives an SSM channel the net.Conn shape an ssh handshake takes; it has no deadlines, and sshHandshake's watchdog is what bounds it.
func asConn(channel io.ReadWriteCloser) net.Conn {
	if conn, ok := channel.(net.Conn); ok {
		return conn
	}

	return rwcConn{channel}
}

type rwcConn struct{ io.ReadWriteCloser }

func (rwcConn) LocalAddr() net.Addr              { return ssmAddr{} }
func (rwcConn) RemoteAddr() net.Addr             { return ssmAddr{} }
func (rwcConn) SetDeadline(time.Time) error      { return nil }
func (rwcConn) SetReadDeadline(time.Time) error  { return nil }
func (rwcConn) SetWriteDeadline(time.Time) error { return nil }
func (ssmAddr) Network() string                  { return "ssm" }
func (ssmAddr) String() string                   { return "ssm" }

type ssmAddr struct{}

// hostKeyAlgorithms are the signature algorithms a pinned key can verify; an RSA key signs as rsa-sha2-*, never as its own type name.
func hostKeyAlgorithms(key ssh.PublicKey) []string {
	if key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}

	return []string{key.Type()}
}
