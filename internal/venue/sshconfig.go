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

// includeDepth caps Include recursion while scanning, matching the parser's
// own limit — the two must agree, or a file one of them reads is a file the
// other never saw.
const includeDepth = 5

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
	hostKey    string
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

	ambient, err := readSSHConfig(worker, host)
	if err != nil {
		return connection{}, err
	}

	conn.dialAt(ambient, host, port)
	conn.fillPaths(ambient)

	return conn, nil
}

// dialAt settles the address and the user, taking the file's answer only where
// the mapping gave none.
func (c *connection) dialAt(ambient ambientSettings, host, port string) {
	if ambient.hostname != "" {
		host = ambient.hostname
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
		c.user = os.Getenv("USER")
	}

	c.address = net.JoinHostPort(host, port)
}

// fillPaths takes the file's key and known_hosts names, expanded against what
// is actually being dialled: %h is the Hostname the file supplied rather than
// the name it was looked up by, which is what OpenSSH substitutes and what a
// per-host key is named after. Runs after dialAt for exactly that reason.
func (c *connection) fillPaths(ambient ambientSettings) {
	host, port := splitHostPort(c.address)

	if len(c.identities) == 0 {
		for _, path := range ambient.identities {
			c.identities = append(c.identities, identity{path: expandPath(path, host, port, c.user)})
		}
	}

	if len(c.knownHosts) == 0 {
		for _, path := range ambient.knownHosts {
			c.knownHosts = append(c.knownHosts, expandPath(path, host, port, c.user))
		}
	}
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

	err = scanUnsupported(path, contents, includeDepth)
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

// resolveAlias reads the five directives of the subset, having first refused
// the three it does not implement.
func resolveAlias(config *ssh_config.Config, alias, path string) (ambientSettings, error) {
	for _, directive := range []string{"ProxyJump", "ProxyCommand", "ProxyUseFdpass"} {
		value, err := config.Get(alias, directive)
		if err != nil {
			return ambientSettings{}, fmt.Errorf("%w: reading %s from %q: %w", errSSHConfig, directive, path, err)
		}

		if value != "" {
			return ambientSettings{}, unsupportedDirective(path, directive, alias)
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

	for _, file := range knownHosts {
		// OpenSSH's "none" means do not check. A feature whose whole job is
		// running commands on another machine does not get to stop checking
		// which machine that is — the same call hostKeyCallback makes about
		// InsecureIgnoreHostKey, for the same reason.
		if strings.EqualFold(file, noKnownHosts) {
			return ambientSettings{}, fmt.Errorf("%w: %q sets UserKnownHostsFile none for %q, and a worker's host key is always checked — pin it with ?hostkey= or name a file with ?known_hosts=",
				errSSHConfig, path, alias)
		}

		settings.knownHosts = append(settings.knownHosts, file)
	}

	return settings, nil
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
// five it honours, and the three it refuses to honour.
var subsetDirectives = map[string]string{
	"hostname":           "HostName",
	"port":               "Port",
	"user":               "User",
	"identityfile":       "IdentityFile",
	"userknownhostsfile": "UserKnownHostsFile",
	"proxyjump":          "ProxyJump",
	"proxycommand":       "ProxyCommand",
	"proxyusefdpass":     "ProxyUseFdpass",
}

// scanUnsupported refuses a file whose Match blocks decide anything this venue
// reads.
//
// Match is resolved APPROXIMATELY here — the parser has no exec, and criteria
// like user and localuser are matched against the alias rather than against
// what they name — so a Match block is a block whose contents may apply to a
// connection this end thinks they do not, or the reverse. That is tolerable
// for a block about agent forwarding and intolerable for one that decides
// which machine gets the step, so the refusal is scoped to blocks naming a
// directive in the subset rather than to Match itself.
//
// Raw text rather than the parsed tree, because an Include's contents are not
// reachable through the parser's API and a Match block inside an included file
// is exactly the one nobody remembers writing.
func scanUnsupported(path string, contents []byte, depth int) error {
	if depth == 0 {
		return nil
	}

	inMatch := false

	for line := range strings.Lines(string(contents)) {
		keyword, value := splitDirective(line)

		switch keyword {
		case "host":
			inMatch = false
		case "match":
			inMatch = true
		case "include":
			err := scanIncluded(value, depth)
			if err != nil {
				return err
			}
		default:
			canonical, reads := subsetDirectives[keyword]
			if inMatch && reads {
				return unsupportedDirective(path, "a Match block naming "+canonical, "")
			}
		}
	}

	return nil
}

// scanIncluded follows an Include the way the parser follows one, so the two
// see the same files.
func scanIncluded(directive string, depth int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	for _, pattern := range strings.Fields(directive) {
		switch {
		case filepath.IsAbs(pattern):
		case strings.HasPrefix(pattern, "~/"):
			pattern = filepath.Join(home, pattern[2:])
		default:
			pattern = filepath.Join(home, ".ssh", pattern)
		}

		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}

		for _, match := range matches {
			contents, err := os.ReadFile(match) //nolint:gosec // a file the operator's own config includes
			if err != nil {
				continue
			}

			err = scanUnsupported(match, contents, depth-1)
			if err != nil {
				return err
			}
		}
	}

	return nil
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
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

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
