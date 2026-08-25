package venue

// The ssh_config subset, against the same in-process sshd the rest of the
// ssh: venue is tested through.
//
// The fixtures always name their own config file. A test that fell through to
// ~/.ssh/config would pass or fail depending on whose machine ran it, and the
// one directive it takes to make that happen — a Host * block — is the
// directive people actually have.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realOpenSSHEnv opts into the conformance test that needs an ssh binary.
const realOpenSSHEnv = "STEPS_TEST_OPENSSH"

// TestSSHConfigResolvesAnAlias is the feature: a worker named the way people
// name machines — an alias out of ~/.ssh/config that is not a hostname — runs
// a step, with every connection detail coming from the file.
func TestSSHConfigResolvesAnAlias(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)
	host, port := hostPortOf(t, server)

	config := writeSSHConfig(t, fmt.Sprintf(`
Host gpu-alias
	HostName %s
	Port %s
	IdentityFile %s
	UserKnownHostsFile %s
`, host, port, server.Identity, server.KnownHosts))

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	spec := sshSpec(t, server, cwd, "out")
	spec.Worker = aliasWorker(t, "gpu-alias", server, config)

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), `echo resolved > out/report.txt`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "out", "report.txt")); !strings.Contains(got, "resolved") {
		t.Errorf("out/report.txt = %q, want the step to have run on the aliased worker", got)
	}
}

// TestSSHConfigSkipsAnUnusableIdentity covers the IdentityFile a Host * block
// leaves lying around: a path that is not there, or a key this process cannot
// read. Ambient names are candidates, and a candidate that cannot be used is
// simply not offered.
func TestSSHConfigSkipsAnUnusableIdentity(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)
	host, port := hostPortOf(t, server)

	junk := filepath.Join(t.TempDir(), "not-a-key")
	mustWrite(t, junk, "this is not a private key\n")

	config := writeSSHConfig(t, fmt.Sprintf(`
Host *
	IdentityFile %s/absent
	IdentityFile %s

Host gpu-alias
	HostName %s
	Port %s
	IdentityFile %s
	UserKnownHostsFile %s
`, t.TempDir(), junk, host, port, server.Identity, server.KnownHosts))

	spec := sshSpec(t, server, t.TempDir())
	spec.Worker = aliasWorker(t, "gpu-alias", server, config)

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestSSHConfigLosesToTheURL pins the precedence, which is the same one
// authMethods already sets between a named identity and an agent: what the
// operator wrote wins, and the file fills in the rest.
//
// With one exception that is the whole point of the feature: the URL's host is
// the name to LOOK UP, not an address that beats the file. `gpu-box` is
// routinely not a hostname, and an alias that resolved to itself would resolve
// to nothing.
func TestSSHConfigLosesToTheURL(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, `
Host box
	HostName elsewhere.invalid
	Port 2200
	User ambient
	IdentityFile /ambient/key
	UserKnownHostsFile /ambient/known_hosts
`)

	worker := mustParseWorker(t, "ssh://named@box:2022?ssh_config="+config+
		"&identity=/named/key&known_hosts=/named/known_hosts")

	conn, err := connectionFor(worker)
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.address != "elsewhere.invalid:2022" {
		t.Errorf("address = %q, want the file's Hostname and the URL's own port", conn.address)
	}

	if conn.user != "named" {
		t.Errorf("user = %q, want the URL's user", conn.user)
	}

	if len(conn.identities) != 1 || conn.identities[0].path != "/named/key" {
		t.Errorf("identities = %+v, want only the one the URL named", conn.identities)
	}

	if len(conn.knownHosts) != 1 || conn.knownHosts[0] != "/named/known_hosts" {
		t.Errorf("knownHosts = %+v, want only the one the URL named", conn.knownHosts)
	}
}

// TestSSHConfigFillsWhatTheURLOmits is the other half of the precedence: with
// nothing but an alias, every answer comes from the file.
func TestSSHConfigFillsWhatTheURLOmits(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, `
Host box
	HostName real.example
	Port 2200
	User ambient
	IdentityFile ~/ambient_key
	UserKnownHostsFile %d/hosts_%h_%p_%r
`)

	worker := mustParseWorker(t, "ssh://box?ssh_config="+config)

	conn, err := connectionFor(worker)
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.address != "real.example:2200" {
		t.Errorf("address = %q, want the file's Hostname and Port", conn.address)
	}

	if conn.user != "ambient" {
		t.Errorf("user = %q, want the file's User", conn.user)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("locating home: %v", err)
	}

	if len(conn.identities) != 1 || conn.identities[0].path != filepath.Join(home, "ambient_key") {
		t.Errorf("identities = %+v, want ~ expanded against home", conn.identities)
	}

	// The tokens resolve against what was dialled, not against the alias:
	// %h is the Hostname the file supplied, not the name it was looked up by.
	want := filepath.Join(home, "hosts_real.example_2200_ambient")
	if len(conn.knownHosts) != 1 || conn.knownHosts[0] != want {
		t.Errorf("knownHosts = %+v, want %q", conn.knownHosts, want)
	}
}

// TestSSHConfigRefusesWhatItCannotHonour is the hazard this whole subset turns
// on. Reading a config file partially is not the same as not reading it: an
// alias whose config routes through a bastion, resolved for its Hostname and
// then dialled directly, runs the step on a machine the operator did not
// authorise — silently, when the direct route happens to work.
func TestSSHConfigRefusesWhatItCannotHonour(t *testing.T) {
	t.Parallel()

	included := filepath.Join(t.TempDir(), "work.conf")
	mustWrite(t, included, "Host box\n\tProxyJump bastion\n")

	tests := map[string]struct {
		config string
		want   string
	}{
		"named for the alias": {
			config: "Host box\n\tProxyJump bastion\n",
			want:   "ProxyJump",
		},
		"a command instead of a socket": {
			config: "Host box\n\tProxyCommand nc %h %p\n",
			want:   "ProxyCommand",
		},
		"through an Include": {
			config: "Include " + included + "\n",
			want:   "ProxyJump",
		},
		// A Match block is refused whether or not this alias appears to select
		// it: only `Match host` and `Match all` are read at all, and neither is
		// resolved the way ssh resolves it, so a block that decides where a
		// connection goes is the one an approximation must not get wrong.
		"inside a Match block": {
			config: "Match user someone-else\n\tProxyJump bastion\n",
			want:   "ProxyJump",
		},
		"a Match block that redirects the host": {
			config: "Match originalhost box\n\tHostName elsewhere.invalid\n",
			want:   "HostName",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			worker := mustParseWorker(t, "ssh://box?ssh_config="+writeSSHConfig(t, test.config))

			_, err := connectionFor(worker)
			if !errors.Is(err, errSSHConfig) {
				t.Fatalf("error = %v, want a refusal", err)
			}

			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v, want it to name %s", err, test.want)
			}
		})
	}
}

// TestSSHConfigAllowsAMatchBlockItDoesNotRead keeps the refusal proportionate.
// Match is common; a Match block that says nothing about where a connection
// goes or how it authenticates cannot change this venue's answer, and refusing
// on it would refuse most real config files for no gain.
func TestSSHConfigAllowsAMatchBlockItDoesNotRead(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, `
Match host anything
	ForwardAgent yes

Host box
	HostName real.example
`)

	worker := mustParseWorker(t, "ssh://box?ssh_config="+config)

	conn, err := connectionFor(worker)
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.address != "real.example:22" {
		t.Errorf("address = %q, want the file to have been read", conn.address)
	}
}

// TestSSHConfigRefusesAFileItCannotRead covers the two ways a file says
// nothing usable. A parse error is a refusal rather than a shrug: the values
// that did not survive it are the ones that decide which machine this is.
func TestSSHConfigRefusesAFileItCannotRead(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path string
		want string
	}{
		// Match exec would run a command to decide, which the parser refuses
		// to do — and a file it cannot parse is a file whose ProxyJump this
		// end cannot see.
		"a directive the parser rejects": {
			path: writeSSHConfig(t, "Match exec \"true\"\n\tForwardAgent yes\n"),
			want: "parsing",
		},
		"a file that is not there": {
			path: filepath.Join(t.TempDir(), "absent"),
			want: "absent",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			worker := mustParseWorker(t, "ssh://box?ssh_config="+test.path)

			_, err := connectionFor(worker)
			if !errors.Is(err, errSSHConfig) {
				t.Fatalf("error = %v, want a refusal", err)
			}

			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v, want it to name %s", err, test.want)
			}
		})
	}
}

// TestSSHConfigNoneReadsNothing is the escape hatch every refusal above points
// at, spelled the way OpenSSH spells it.
func TestSSHConfigNoneReadsNothing(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host box\n\tProxyJump bastion\n\tHostName elsewhere.invalid\n")

	worker := mustParseWorker(t, "ssh://box?ssh_config=none&known_hosts="+config)

	conn, err := connectionFor(worker)
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.address != "box:22" {
		t.Errorf("address = %q, want the mapping as written", conn.address)
	}
}

// TestSSHConfigDefaultFileIsOptional pins that a machine with no ~/.ssh/config
// — a container, a CI runner — dials exactly as it did before this existed.
func TestSSHConfigDefaultFileIsOptional(t *testing.T) {
	// Not parallel: it moves HOME, which is where the default file is looked
	// for, so that this test cannot read the config of whoever runs it.
	t.Setenv("HOME", t.TempDir())

	worker := mustParseWorker(t, "ssh://box?known_hosts=/somewhere/known_hosts")

	conn, err := connectionFor(worker)
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.address != "box:22" {
		t.Errorf("address = %q, want the mapping as written", conn.address)
	}
}

// TestSSHConfigRefusesKnownHostsNone is the one directive in the subset whose
// honest reading this venue will not perform. OpenSSH's `UserKnownHostsFile
// none` means do not check; a thing whose whole job is running commands on
// another machine does not get to stop checking which machine that is.
func TestSSHConfigRefusesKnownHostsNone(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host box\n\tUserKnownHostsFile none\n")

	worker := mustParseWorker(t, "ssh://box?ssh_config="+config)

	_, err := connectionFor(worker)
	if !errors.Is(err, errSSHConfig) {
		t.Fatalf("error = %v, want a refusal", err)
	}
}

// TestSSHConfigKnownHostsIsAList covers the shape OpenSSH's own default has:
// one UserKnownHostsFile naming several files. Taken whole it is a name no
// file has, and every dial fails on opening it.
func TestSSHConfigKnownHostsIsAList(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host box\n\tUserKnownHostsFile /a/known_hosts /b/known_hosts\n")

	conn, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if len(conn.knownHosts) != 2 || conn.knownHosts[0] != "/a/known_hosts" || conn.knownHosts[1] != "/b/known_hosts" {
		t.Errorf("knownHosts = %+v, want both files", conn.knownHosts)
	}
}

// TestSSHConfigKnownHostsNoneLosesToAPin keeps the refusal to the question it
// answers. A mapping that pins the key has already said which machine this is,
// and refusing it with "pin it with ?hostkey=" tells an operator to do what
// they did.
func TestSSHConfigKnownHostsNoneLosesToAPin(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host *\n\tUserKnownHostsFile none\n")
	pin := "SHA256:" + strings.Repeat("A", 43)

	conn, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config+"&hostkey="+pin))
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.hostKey != pin {
		t.Errorf("hostKey = %q, want the pin the mapping named", conn.hostKey)
	}
}

// TestSSHConfigHonoursOptingOutOfAProxy covers `ProxyJump none`, which is how
// one host says it is NOT behind the bastion a wildcard block put everything
// behind. Refusing it refuses the one spelling that means "dial me directly".
func TestSSHConfigHonoursOptingOutOfAProxy(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host box\n\tProxyJump none\n\tProxyCommand none\n\tHostName real.example\n\nHost *\n\tProxyJump bastion\n")

	conn, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.address != "real.example:22" {
		t.Errorf("address = %q, want the host that opted out of the proxy", conn.address)
	}
}

// TestSSHConfigExpandsHostNameTokens covers `HostName %h.internal`, which is
// most of why a short alias is written at all. Unexpanded it is a literal that
// resolves to nothing.
func TestSSHConfigExpandsHostNameTokens(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host box\n\tHostName %h.internal.example\n")

	conn, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.address != "box.internal.example:22" {
		t.Errorf("address = %q, want %%h replaced by the alias", conn.address)
	}
}

// TestSSHConfigRefusesAHiddenMatchBlock is the refusal held against the two
// ways a Match block hides from a scan: behind an Include of its own, and at
// the bottom of a chain of them. Both were honoured and neither was refused.
func TestSSHConfigRefusesAHiddenMatchBlock(t *testing.T) {
	t.Parallel()

	tests := map[string]func(t *testing.T) string{
		"an Include inside the Match block": func(t *testing.T) string {
			t.Helper()

			inner := filepath.Join(t.TempDir(), "inner.conf")
			mustWrite(t, inner, "HostName elsewhere.invalid\n")

			return writeSSHConfig(t, "Match host box\n\tInclude "+inner+"\n")
		},
		// As deep as the parser will follow one. A scan that stopped a level
		// short read a different file set than the parser did, which is the
		// one thing the two must not do.
		"as deep as the parser reads": func(t *testing.T) string {
			t.Helper()

			dir := t.TempDir()
			for level := 1; level <= 5; level++ {
				body := "Include " + filepath.Join(dir, fmt.Sprintf("l%d.conf", level+1)) + "\n"
				if level == 5 {
					body = "Match host box\n\tHostName elsewhere.invalid\n"
				}

				mustWrite(t, filepath.Join(dir, fmt.Sprintf("l%d.conf", level)), body)
			}

			return writeSSHConfig(t, "Include "+filepath.Join(dir, "l1.conf")+"\n")
		},
	}

	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			worker := mustParseWorker(t, "ssh://box?ssh_config="+fixture(t))

			_, err := connectionFor(worker)
			if !errors.Is(err, errSSHConfig) {
				t.Fatalf("error = %v, want a refusal naming the Match block", err)
			}
		})
	}
}

// TestSSHConfigRefusesARewrittenHostname covers CanonicalizeHostname, which
// turns a short name into a different one — the same hazard as a bastion, and
// silent for the same reason.
func TestSSHConfigRefusesARewrittenHostname(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host box\n\tCanonicalizeHostname yes\n\tCanonicalDomains internal.example\n")

	_, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
	if !errors.Is(err, errSSHConfig) {
		t.Fatalf("error = %v, want a refusal", err)
	}
}

// TestSSHConfigAgainstRealOpenSSH is the only check that can catch this subset
// drifting from the thing it imitates: OpenSSH resolves the same file, and
// `ssh -G` prints what it decided. Opt-in, because it needs an ssh binary and
// a version whose defaults match the assumptions here.
func TestSSHConfigAgainstRealOpenSSH(t *testing.T) {
	if os.Getenv(realOpenSSHEnv) == "" {
		t.Skipf("set %s=1 to check this subset against the local ssh(1)", realOpenSSHEnv)
	}

	config := writeSSHConfig(t, `
Host gpu
	HostName gpu.example
	Port 2222
	User jt
	IdentityFile /keys/gpu

Host *
	User fallback
`)

	worker := mustParseWorker(t, "ssh://gpu?ssh_config="+config)

	conn, err := connectionFor(worker)
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	printed := sshDashG(t, config, "gpu")

	want := net.JoinHostPort(printed["hostname"], printed["port"])
	if conn.address != want {
		t.Errorf("address = %q, ssh -G resolved %q", conn.address, want)
	}

	if conn.user != printed["user"] {
		t.Errorf("user = %q, ssh -G resolved %q", conn.user, printed["user"])
	}

	if len(conn.identities) == 0 || conn.identities[0].path != printed["identityfile"] {
		t.Errorf("identities = %+v, ssh -G resolved %q first", conn.identities, printed["identityfile"])
	}
}

// sshDashG asks the local ssh what it would do with a config, as a flat map of
// its first answer per keyword.
func sshDashG(t *testing.T, config, alias string) map[string]string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "ssh", "-G", "-F", config, alias).Output() //nolint:gosec // the local ssh, asked about a fixture this test wrote
	if err != nil {
		t.Fatalf("ssh -G: %v", err)
	}

	resolved := map[string]string{}

	for line := range strings.Lines(string(out)) {
		key, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}

		if _, seen := resolved[key]; !seen {
			resolved[key] = value
		}
	}

	return resolved
}

// writeSSHConfig puts a fixture on disk and hands back its path.
func writeSSHConfig(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config")
	mustWrite(t, path, contents)

	return path
}

// aliasWorker is a mapping that names the test server by an alias only the
// config file can resolve.
func aliasWorker(t *testing.T, alias string, server *testSSHD, config string) string {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	return fmt.Sprintf("ssh://%s%s?ssh_config=%s&binary=%s", alias, server.Root, config, self)
}

func hostPortOf(t *testing.T, server *testSSHD) (string, string) {
	t.Helper()

	host, port, err := net.SplitHostPort(server.listener.Addr().String())
	if err != nil {
		t.Fatalf("splitting the server address: %v", err)
	}

	return host, port
}

func mustParseWorker(t *testing.T, raw string) Worker {
	t.Helper()

	worker, err := ParseWorker(raw)
	if err != nil {
		t.Fatalf("ParseWorker(%q): %v", raw, err)
	}

	return worker
}

// TestSSHConfigMatchesTheAliasCaseInsensitively pins what ssh(1) does before
// it reads the file at all: the name on the command line is lowercased. A
// case-differing alias that silently skipped its own Host block would also
// skip that block's ProxyJump — the exact partial read the refusals exist to
// prevent.
func TestSSHConfigMatchesTheAliasCaseInsensitively(t *testing.T) {
	t.Parallel()

	t.Run("resolves", func(t *testing.T) {
		t.Parallel()

		config := writeSSHConfig(t, "Host gpu-box\n\tHostName real.example\n")

		conn, err := connectionFor(mustParseWorker(t, "ssh://GPU-Box?ssh_config="+config))
		if err != nil {
			t.Fatalf("connectionFor: %v", err)
		}

		if conn.address != "real.example:22" {
			t.Errorf("address = %q, want the block ssh would have matched", conn.address)
		}
	})

	t.Run("refuses", func(t *testing.T) {
		t.Parallel()

		config := writeSSHConfig(t, "Host gpu-box\n\tProxyJump bastion\n")

		_, err := connectionFor(mustParseWorker(t, "ssh://GPU-Box?ssh_config="+config))
		if !errors.Is(err, errSSHConfig) {
			t.Fatalf("error = %v, want the refusal a case difference must not sidestep", err)
		}
	})
}

// TestSSHConfigHonoursBooleanOffSpellings covers the synonym OpenSSH accepts:
// `CanonicalizeHostname false` turns canonicalization off exactly as `no`
// does, and refusing it refuses a file that changes nothing.
func TestSSHConfigHonoursBooleanOffSpellings(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host box\n\tCanonicalizeHostname false\n\tProxyUseFdpass false\n\tHostName real.example\n")

	conn, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	if conn.address != "real.example:22" {
		t.Errorf("address = %q, want the host whose booleans are off", conn.address)
	}
}

// TestSSHConfigRefusesDevNullKnownHosts covers the common spelling of the same
// intent `none` declares: a file that is always empty checks nothing, and the
// refusal has to name the directive rather than fail every dial with "host
// unknown" and no cause an operator can act on.
func TestSSHConfigRefusesDevNullKnownHosts(t *testing.T) {
	t.Parallel()

	config := writeSSHConfig(t, "Host box\n\tUserKnownHostsFile /dev/null\n")

	_, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
	if !errors.Is(err, errSSHConfig) {
		t.Fatalf("error = %v, want a refusal naming the directive", err)
	}
}

// TestSSHConfigKnownHostsWithSpaces covers a path the parser hands over with
// its quotes already stripped, and one still carrying them: both name files,
// not the fragments a whitespace split would leave.
func TestSSHConfigKnownHostsWithSpaces(t *testing.T) {
	t.Parallel()

	t.Run("quoted beside another", func(t *testing.T) {
		t.Parallel()

		config := writeSSHConfig(t, "Host box\n\tUserKnownHostsFile \"/a b/known_hosts\" /c/known_hosts\n")

		conn, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
		if err != nil {
			t.Fatalf("connectionFor: %v", err)
		}

		want := []string{"/a b/known_hosts", "/c/known_hosts"}
		if len(conn.knownHosts) != 2 || conn.knownHosts[0] != want[0] || conn.knownHosts[1] != want[1] {
			t.Errorf("knownHosts = %+v, want %v", conn.knownHosts, want)
		}
	})

	t.Run("fully quoted", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "with space")
		mustMkdir(t, dir)
		hosts := filepath.Join(dir, "known_hosts")
		mustWrite(t, hosts, "")

		config := writeSSHConfig(t, "Host box\n\tUserKnownHostsFile \""+hosts+"\"\n")

		conn, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
		if err != nil {
			t.Fatalf("connectionFor: %v", err)
		}

		if len(conn.knownHosts) != 1 || conn.knownHosts[0] != hosts {
			t.Errorf("knownHosts = %+v, want the one file the value names", conn.knownHosts)
		}
	})
}

// TestSSHConfigRefusesADirectiveAnIncludeUnanchors is the block boundary the
// parser and OpenSSH disagree about: a Host or Match block opened inside an
// included file scopes the parent's next lines for ssh, but not for the
// parser, which reads each included file on its own. A subset directive
// sitting in that disputed stretch is refused until a Host or Match line
// re-anchors both readers.
func TestSSHConfigRefusesADirectiveAnIncludeUnanchors(t *testing.T) {
	t.Parallel()

	t.Run("refused in the disputed stretch", func(t *testing.T) {
		t.Parallel()

		included := filepath.Join(t.TempDir(), "trailing.conf")
		mustWrite(t, included, "Match host special\n\tForwardAgent yes\n")

		config := writeSSHConfig(t, "Include "+included+"\nHostName elsewhere.invalid\n")

		_, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
		if !errors.Is(err, errSSHConfig) {
			t.Fatalf("error = %v, want a refusal naming HostName", err)
		}

		if !strings.Contains(err.Error(), "HostName") {
			t.Errorf("error = %v, want it to name HostName", err)
		}
	})

	t.Run("re-anchored by the next Host line", func(t *testing.T) {
		t.Parallel()

		included := filepath.Join(t.TempDir(), "hosts.conf")
		mustWrite(t, included, "Host other\n\tHostName other.example\n")

		config := writeSSHConfig(t, "Include "+included+"\nHost box\n\tHostName real.example\n")

		conn, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
		if err != nil {
			t.Fatalf("connectionFor: %v", err)
		}

		if conn.address != "real.example:22" {
			t.Errorf("address = %q, want the re-anchored block honoured", conn.address)
		}
	})
}

// TestSSHConfigNamesTheKeysItSkipped keeps the skip diagnosable: a candidate
// key the file named and this end could not use is exactly the answer to "why
// did nothing authenticate", and an error that never mentions it sends the
// operator digging with strace.
func TestSSHConfigNamesTheKeysItSkipped(t *testing.T) {
	// Not parallel: it clears the agent from the environment.
	t.Setenv("SSH_AUTH_SOCK", "")

	absent := filepath.Join(t.TempDir(), "gpu_ed25519")
	config := writeSSHConfig(t, "Host box\n\tIdentityFile "+absent+"\n")

	settings, err := connectionFor(mustParseWorker(t, "ssh://box?ssh_config="+config))
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	_, err = authMethods(context.Background(), settings)
	if !errors.Is(err, errNoAuth) {
		t.Fatalf("error = %v, want the missing credentials", err)
	}

	if !strings.Contains(err.Error(), absent) {
		t.Errorf("error = %v, want it to name the skipped candidate %q", err, absent)
	}
}
