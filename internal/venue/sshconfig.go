package venue

// A documented subset of ssh_config, so a worker can be named the way people
// name machines.
//
// `--worker gpu=ssh://jt@gpu-box:2222?identity=/keys/gpu` makes an operator
// restate in a flag what ~/.ssh/config already says, and often it cannot be
// restated at all: an alias is frequently not a hostname. The file answers
// five questions this venue would otherwise guess at — and explicit still
// beats ambient, the precedent authMethods already set between a named
// identity and an agent.
//
// The subset is documented because it is a subset. Reading a config file
// PARTIALLY is not the same as not reading it: an alias whose config routes
// through a bastion, resolved for its Hostname and then dialled directly, runs
// the step on a machine the operator did not authorise — and on a network
// where the direct route happens to work, silently. So anything outside the
// subset that decides where a connection goes is refused, by name, rather than
// skipped.

import (
	"errors"
	"fmt"
	"net"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// errSSHConfig is an ssh_config this venue will not act on as written.
var errSSHConfig = errors.New("ssh_config")

// noSSHConfig is ?ssh_config=none: dial the mapping exactly as written. Spelt
// the way OpenSSH's -F spells it, and it is the answer every refusal below
// points at.
const noSSHConfig = "none"

// noKnownHosts is UserKnownHostsFile's "don't check" value, which is the one
// directive in the subset this venue will not honour.
const noKnownHosts = "none"

// identity is a private key to offer, and whether the operator named it.
//
// The distinction decides what an unusable key means. A key named in the URL
// that cannot be read is an error: it is what the operator said to use, and
// silently falling back to an agent would answer a question they had already
// answered. A key named by a config file is a CANDIDATE — a Host * block
// routinely names one that is absent on this machine, or encrypted, and
// OpenSSH simply moves on.
type identity struct {
	path     string
	explicit bool
}

// connection is a worker's dial settings once the config has filled in
// whatever the URL left out.
//
// Separate from Worker deliberately: Worker is the mapping as the operator
// wrote it, which is what errors quote and what a run record and the web UI
// draw. Resolving an alias must not rewrite that — `gpu-box` is the name they
// used, and an address they never typed is a worse answer to "where did this
// step run".
type connection struct {
	worker     Worker
	address    string
	user       string
	identities []identity
	// knownHosts is empty when neither the URL nor the config named a file,
	// leaving hostKeyCallback's own default in place.
	knownHosts []string
	// ambientHosts says knownHosts came from the config rather than the URL.
	// A file a config names per host may simply not exist yet, and OpenSSH
	// reads an absent known_hosts as empty rather than failing on it — but
	// falling back to ~/.ssh/known_hosts would consult a file the operator's
	// own config excluded, so the two cases cannot share a code path.
	ambientHosts bool
	hostKey      string
}

// connectionFor works out how to reach a worker.
func connectionFor(worker Worker) (connection, error) {
	conn := connection{worker: worker, user: worker.User, hostKey: worker.HostKey}

	if worker.Identity != "" {
		conn.identities = []identity{{path: worker.Identity, explicit: true}}
	}

	if worker.KnownHosts != "" {
		conn.knownHosts = []string{worker.KnownHosts}
	}

	// The URL's host is the name to LOOK UP, not an address the file loses to:
	// an alias that resolved to itself would resolve to nothing, which is the
	// whole reason for reading the file.
	host, port := splitHostPort(worker.Host)

	// Lowercased first, because ssh(1) lowercases the name it was given before
	// the file ever sees it. GPU-Box and gpu-box are the same machine to a
	// config — and to the ProxyJump refusal, which a case difference must not
	// sidestep by silently missing the block.
	host = strings.ToLower(host)

	ambient, err := readSSHConfig(worker, host)
	if err != nil {
		return connection{}, err
	}

	conn.dialAt(ambient, host, port)

	err = conn.fillPaths(ambient, host)
	if err != nil {
		return connection{}, err
	}

	return conn, nil
}

// dialAt settles the address and the user, taking the file's answer only where
// the mapping gave none.
func (c *connection) dialAt(ambient ambientSettings, host, port string) {
	if ambient.hostname != "" {
		// %h inside a HostName is the name it was looked up BY, which is what
		// makes `HostName %h.internal` — the reason half these files exist —
		// name a machine rather than a literal percent sign.
		host = expandHostname(ambient.hostname, host)
	}

	if port == "" {
		port = ambient.port
	}

	if port == "" {
		port = defaultSSHPort
	}

	if c.user == "" {
		c.user = ambient.user
	}

	if c.user == "" {
		c.user = localUser()
	}

	c.address = net.JoinHostPort(host, port)
}

// fillPaths takes the file's key and known_hosts names, expanded against what
// is actually being dialled: %h is the Hostname the file supplied rather than
// the name it was looked up by, which is what OpenSSH substitutes and what a
// per-host key is named after. Runs after dialAt for exactly that reason.
func (c *connection) fillPaths(ambient ambientSettings, alias string) error {
	host, port := splitHostPort(c.address)

	if len(c.identities) == 0 {
		for _, path := range ambient.identities {
			c.identities = append(c.identities, identity{path: expandPath(path, host, port, c.user)})
		}
	}

	// A pin, or a file the URL named, is already an answer about which key to
	// expect. What the config says about known_hosts cannot change it —
	// including its refusal to have one, which is why the refusal below is
	// here rather than where the file is read.
	if c.hostKey != "" || len(c.knownHosts) > 0 {
		return nil
	}

	for _, path := range ambient.knownHosts {
		// OpenSSH's "none" means do not check, and /dev/null is the common
		// spelling of the same intent: a file that is always empty knows no
		// host, so every dial would fail as "host unknown" with no cause an
		// operator can act on. A feature whose whole job is running commands
		// on another machine does not get to stop checking which machine that
		// is — the same call hostKeyCallback makes about
		// InsecureIgnoreHostKey, for the same reason.
		if strings.EqualFold(path, noKnownHosts) || path == os.DevNull {
			return fmt.Errorf("%w: UserKnownHostsFile none is set for %q, and a worker's host key is always checked — pin it with ?hostkey= or name a file with ?known_hosts=",
				errSSHConfig, alias)
		}

		c.knownHosts = append(c.knownHosts, expandPath(path, host, port, c.user))
	}

	c.ambientHosts = len(c.knownHosts) > 0

	return nil
}

// ambientSettings is what a config file had to say about one alias.
type ambientSettings struct {
	hostname   string
	port       string
	user       string
	identities []string
	knownHosts []string
}

// readSSHConfig resolves one alias against the operator's config, or returns
// nothing at all when there is no file to read.
func readSSHConfig(worker Worker, alias string) (ambientSettings, error) {
	path, named := sshConfigPath(worker)
	if path == "" {
		return ambientSettings{}, nil
	}

	contents, err := os.ReadFile(path) //nolint:gosec // the operator's own ssh config, read on their behalf
	if err != nil {
		// A file the operator NAMED and this end cannot read is a mistake
		// worth reporting; the default one simply not being there is a
		// container, a CI runner, a machine that has never run ssh.
		if !named && errors.Is(err, os.ErrNotExist) {
			return ambientSettings{}, nil
		}

		return ambientSettings{}, fmt.Errorf("%w: reading %q: %w", errSSHConfig, path, err)
	}

	_, _, err = scanUnsupported(path, contents, map[string]bool{path: true}, scanState{})
	if err != nil {
		return ambientSettings{}, err
	}

	config, err := ssh_config.DecodeBytes(contents)
	if err != nil {
		// Including Match exec, which the parser refuses to evaluate because
		// evaluating it means running a command. A file this end cannot parse
		// is a file whose ProxyJump it cannot see.
		return ambientSettings{}, fmt.Errorf("%w: parsing %q (%w) — dial it as written with ?ssh_config=none",
			errSSHConfig, path, err)
	}

	return resolveAlias(config, alias, path)
}

// refusedDirectives decide where a connection goes, or which key proves the
// machine at the other end is the right one, and this venue implements none of
// them. Each carries the values that mean "not set", because a host opting
// itself out of a wildcard block — `ProxyJump none` under a `Host *` that sets
// one — is saying dial me directly, which is exactly what this does. A boolean
// is off as `no` or as `false`: OpenSSH accepts both spellings, and refusing
// one of them refuses a file that changes nothing.
var refusedDirectives = []struct {
	directive string
	offs      []string
}{
	{"ProxyJump", []string{"none"}},
	{"ProxyCommand", []string{"none"}},
	{"ProxyUseFdpass", []string{"no", "false"}},
	// CanonicalizeHostname turns a short name into a different one by
	// appending CanonicalDomains, which is the same hazard as a bastion: a
	// name resolved one way here and another way by ssh reaches two machines.
	{"CanonicalizeHostname", []string{"no", "false"}},
	// HostKeyAlias says which known_hosts entry proves the host. Ignoring it
	// would check a real machine against the wrong recorded key and report a
	// mismatch — the alarm this venue exists to raise, raised falsely.
	{"HostKeyAlias", nil},
}

// resolveAlias reads the five directives of the subset, having first refused
// the ones it does not implement.
func resolveAlias(config *ssh_config.Config, alias, path string) (ambientSettings, error) {
	for _, refused := range refusedDirectives {
		value, err := config.Get(alias, refused.directive)
		if err != nil {
			return ambientSettings{}, fmt.Errorf("%w: reading %s from %q: %w", errSSHConfig, refused.directive, path, err)
		}

		if value != "" && !containsFold(refused.offs, value) {
			return ambientSettings{}, unsupportedDirective(path, refused.directive, alias)
		}
	}

	settings := ambientSettings{}

	for _, resolve := range []struct {
		directive string
		into      *string
	}{
		{"Hostname", &settings.hostname},
		{"Port", &settings.port},
		{"User", &settings.user},
	} {
		value, err := config.Get(alias, resolve.directive)
		if err != nil {
			return ambientSettings{}, fmt.Errorf("%w: reading %s from %q: %w", errSSHConfig, resolve.directive, path, err)
		}

		*resolve.into = value
	}

	// All of them, in order, the way OpenSSH offers every IdentityFile it was
	// given rather than only the last.
	identities, err := config.GetAll(alias, "IdentityFile")
	if err != nil {
		return ambientSettings{}, fmt.Errorf("%w: reading IdentityFile from %q: %w", errSSHConfig, path, err)
	}

	settings.identities = identities

	knownHosts, err := config.GetAll(alias, "UserKnownHostsFile")
	if err != nil {
		return ambientSettings{}, fmt.Errorf("%w: reading UserKnownHostsFile from %q: %w", errSSHConfig, path, err)
	}

	// One UserKnownHostsFile names a LIST — OpenSSH's own default is two files
	// — so the value is fields, not a path. Taken whole it becomes one name
	// that cannot exist, and every dial fails on opening it.
	for _, file := range knownHosts {
		settings.knownHosts = append(settings.knownHosts, splitPaths(file)...)
	}

	return settings, nil
}

// containsFold reports whether value is one of values, ignoring case.
func containsFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}

	return false
}

// splitPaths reads one UserKnownHostsFile value as the list OpenSSH reads it
// as: whitespace-separated, with quotes carrying a path with spaces through
// the split.
//
// The parser strips a value's OUTER quotes before this sees it, which leaves
// `"/a b/hosts"` indistinguishable from two names — except by looking: a whole
// value that names an existing file is one path. A single space-bearing path
// that does not exist yet still splits wrongly, which is the parser's
// ambiguity, not one more stat can resolve.
func splitPaths(value string) []string {
	if !strings.Contains(value, `"`) {
		if strings.Contains(value, " ") {
			_, err := os.Stat(value) //nolint:gosec // a path the operator's own config names
			if err == nil {
				return []string{value}
			}
		}

		return strings.Fields(value)
	}

	var paths []string

	for i, part := range strings.Split(value, `"`) {
		if i%2 == 1 {
			// Between quotes: one path, spaces and all.
			if part != "" {
				paths = append(paths, part)
			}

			continue
		}

		paths = append(paths, strings.Fields(part)...)
	}

	return paths
}

// sshConfigPath is the file to read, and whether the operator named it.
func sshConfigPath(worker Worker) (string, bool) {
	if strings.EqualFold(worker.SSHConfig, noSSHConfig) {
		return "", false
	}

	if worker.SSHConfig != "" {
		return worker.SSHConfig, true
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}

	// The user file only. /etc/ssh/ssh_config is deliberately not read: host
	// aliases live in the user file, and the parser resolves a relative
	// Include inside a system file against ~/.ssh anyway — reading it would
	// mean following includes to files OpenSSH would not have read.
	return filepath.Join(home, ".ssh", "config"), false
}

// subsetDirectives are the keywords this venue acts on, canonically spelt: the
// five it honours, and the five it refuses to honour.
var subsetDirectives = map[string]string{
	"hostname":             "HostName",
	"port":                 "Port",
	"user":                 "User",
	"identityfile":         "IdentityFile",
	"userknownhostsfile":   "UserKnownHostsFile",
	"proxyjump":            "ProxyJump",
	"proxycommand":         "ProxyCommand",
	"proxyusefdpass":       "ProxyUseFdpass",
	"canonicalizehostname": "CanonicalizeHostname",
	"hostkeyalias":         "HostKeyAlias",
}

// scanState is where a scan stands in a file: inside a Match block, or in the
// stretch after an Include whose file opened blocks of its own — where the
// parser and OpenSSH disagree about which block the next directive belongs to.
type scanState struct {
	inMatch    bool
	unanchored bool
}

// scanUnsupported refuses a file whose Match blocks decide anything this venue
// reads.
//
// Match is resolved APPROXIMATELY here — `Match host` and `Match all` are all
// the parser implements, and it matches them against the alias rather than
// against the resolved hostname — so a Match block is a block whose contents
// may apply to a connection this end thinks they do not, or the reverse. That
// is tolerable for a block about agent forwarding and intolerable for one that
// decides which machine gets the step, so the refusal is scoped to blocks
// naming a directive in the subset rather than to Match itself. (Every other
// criterion — exec, user, localuser, final — fails to parse instead, and a
// file this end cannot parse is refused whole.)
//
// Raw text rather than the parsed tree, because an Include's contents are not
// reachable through the parser's API and a Match block inside an included file
// is exactly the one nobody remembers writing. Includes are followed with a
// visited set rather than the parser's own depth cap: scanning MORE than the
// parser reads can only refuse, never silently honour, so the scan stays a
// superset of whatever a parser version follows. (A chain deeper than the
// parser's cap fails to parse anyway, and a file this end cannot parse is
// refused whole.)
//
// The returned state is the file's TRAILING one, and the bool says whether the
// file opened any Host or Match block — because OpenSSH reads an Include
// textually, so a block opened inside an included file scopes the parent's
// next lines, while the parser reads each included file on its own and does
// not. A subset directive in that disputed stretch is refused until a Host or
// Match line re-anchors both readers.
func scanUnsupported(path string, contents []byte, visited map[string]bool, state scanState) (scanState, bool, error) {
	sawBlock := false

	for line := range strings.Lines(string(contents)) {
		keyword, value := splitDirective(line)

		switch keyword {
		case "host":
			state = scanState{}
			sawBlock = true
		case "match":
			state = scanState{inMatch: true}
			sawBlock = true
		case "include":
			// Carrying the state across the Include: an Include INSIDE a
			// Match block applies on that block's terms, so what it names is
			// what the block names -- and a scan that reset the flag let a
			// Match block redirect a worker's HostName through one line of
			// indirection.
			opened, err := scanIncluded(value, visited, state)
			if err != nil {
				return state, sawBlock, err
			}

			if opened {
				sawBlock = true

				state.unanchored = true
			}
		default:
			err := refuseDisputedScope(path, keyword, state)
			if err != nil {
				return state, sawBlock, err
			}
		}
	}

	return state, sawBlock, nil
}

// refuseDisputedScope is the scan's verdict on one directive: refused when it
// is in the subset and sits where the parser and OpenSSH disagree about which
// block owns it.
func refuseDisputedScope(path, keyword string, state scanState) error {
	canonical, reads := subsetDirectives[keyword]
	if !reads {
		return nil
	}

	if state.inMatch {
		return unsupportedDirective(path, "a Match block naming "+canonical, "")
	}

	if state.unanchored {
		return unsupportedDirective(path, canonical+" after an Include that opens a Host or Match block", "")
	}

	return nil
}

// includePath resolves one Include pattern the way the parser resolves it.
func includePath(pattern, home string) string {
	switch {
	case filepath.IsAbs(pattern):
		return pattern
	case strings.HasPrefix(pattern, "~/"):
		return filepath.Join(home, pattern[2:])
	default:
		return filepath.Join(home, ".ssh", pattern)
	}
}

// scanIncluded follows an Include the way the parser follows one, so the scan
// sees at least the files the parser reads. It reports whether any included
// file opened a Host or Match block, and chains each file's trailing state
// into the next — glob order is read order for both readers, and OpenSSH's
// textual reading lets one file's trailing block scope the next file's lines.
func scanIncluded(directive string, visited map[string]bool, state scanState) (bool, error) {
	home := sshHome()
	opened := false

	for _, pattern := range strings.Fields(directive) {
		matches, err := filepath.Glob(includePath(pattern, home))
		if err != nil {
			continue
		}

		for _, match := range matches {
			if visited[match] {
				continue
			}

			visited[match] = true

			contents, err := os.ReadFile(match) //nolint:gosec // a file the operator's own config includes
			if err != nil {
				continue
			}

			trailing, saw, err := scanUnsupported(match, contents, visited, state)
			if err != nil {
				return opened, err
			}

			if saw {
				opened = true
			}

			state = trailing
			if opened {
				state.unanchored = true
			}
		}
	}

	return opened, nil
}

// splitDirective reads one config line as a lowercased keyword and its value.
func splitDirective(line string) (string, string) {
	if comment := strings.IndexByte(line, '#'); comment >= 0 {
		line = line[:comment]
	}

	line = strings.TrimSpace(line)

	// `Host box` and `Host=box` mean the same thing to OpenSSH.
	separator := strings.IndexAny(line, " \t=")
	if separator < 0 {
		return strings.ToLower(line), ""
	}

	return strings.ToLower(line[:separator]), strings.Trim(line[separator:], " \t=")
}

// unsupportedDirective is the refusal, naming what was found and where.
func unsupportedDirective(path, directive, alias string) error {
	where := ""
	if alias != "" {
		where = " for " + alias
	}

	return fmt.Errorf("%w: %q sets %s%s, which this subset does not implement — reach the worker directly, or dial the mapping as written with ?ssh_config=none",
		errSSHConfig, path, directive, where)
}

// expandPath resolves the tokens OpenSSH substitutes into a path.
//
// Without them a `%d/keys/%h` never names a file that exists, which for an
// IdentityFile means silently offering nothing — the failure mode this whole
// feature is meant to remove.
func expandPath(raw, host, port, user string) string {
	home := sshHome()

	if strings.HasPrefix(raw, "~/") {
		raw = filepath.Join(home, raw[2:])
	}

	return strings.NewReplacer(
		"%%", "%",
		"%d", home,
		"%h", host,
		"%p", port,
		"%r", user,
	).Replace(raw)
}

// expandHostname resolves the tokens OpenSSH substitutes into a HostName,
// where %h is the name the alias was looked up BY rather than the one being
// produced. `HostName %h.internal` is the shape this exists for; unexpanded it
// is a literal that resolves to nothing.
func expandHostname(raw, alias string) string {
	return strings.NewReplacer("%%", "%", "%h", alias).Replace(raw)
}

// localUser is the account name to log in as when neither the mapping nor the
// config named one, worked out the way sshHome is and for the same reason.
//
// $USER only is not that: it is unset under systemd units, in many containers,
// and after su without a login shell — and an empty ClientConfig.User fails
// authentication against a machine `ssh box` reaches fine, with an error that
// names no cause.
func localUser() string {
	current, err := osuser.Current()
	if err == nil && current.Username != "" {
		return current.Username
	}

	return os.Getenv("USER")
}

// sshHome is the home directory OpenSSH resolves ~ and %d against, worked out
// the way the parser does it.
//
// The passwd entry first, $HOME only as a fallback: that is what ssh(1) itself
// uses, and — the reason it cannot just be os.UserHomeDir — it is what
// ssh_config's own Include resolution uses. Anywhere the two disagree, a
// relative Include would be scanned in one directory and parsed from another,
// which is a file the parser reads and this end never saw.
func sshHome() string {
	current, err := osuser.Current()
	if err == nil && current.HomeDir != "" {
		return current.HomeDir
	}

	return os.Getenv("HOME")
}

// splitHostPort reads a worker URL's host, whose port is optional.
func splitHostPort(authority string) (string, string) {
	host, port, err := net.SplitHostPort(authority)
	if err == nil {
		return host, port
	}

	// An IPv6 literal with no port arrives bracketed, and JoinHostPort would
	// bracket it again.
	return strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]"), ""
}
