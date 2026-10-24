package shell

import (
	"fmt"
	"strings"
)

//nolint:gochecknoglobals // the one sandboxer this OS has
var hostSandbox sandboxer = seatbelt{}

// seatbelt fences a command with sandbox-exec. Deprecated by Apple since 10.12 and still shipped (verified on macOS 27); Claude Code and Codex depend on it too. Paths travel as -D parameters rather than in the profile text, so no path needs SBPL escaping.
type seatbelt struct{}

// seatbeltDevices are the device nodes an ordinary shell writes: redirections to /dev/null, its terminal, its own fds.
const seatbeltDevices = `(literal "/dev/null") (literal "/dev/zero") (literal "/dev/tty") (literal "/dev/dtracehelper") (regex #"^/dev/fd/") (regex #"^/dev/ttys")`

func (seatbelt) command(policy sandboxPolicy, argv []string) ([]string, error) {
	args := []string{"/usr/bin/sandbox-exec"}

	var profile strings.Builder

	profile.WriteString("(version 1)(allow default)(deny file-write*)(allow file-write* " + seatbeltDevices)

	for i, dir := range policy.writable {
		key := fmt.Sprintf("W%d", i)
		args = append(args, "-D", key+"="+dir)
		fmt.Fprintf(&profile, ` (subpath (param %q))`, key)
	}

	profile.WriteString(")")

	for i, path := range policy.denyRead {
		key := fmt.Sprintf("R%d", i)
		args = append(args, "-D", key+"="+path)
		fmt.Fprintf(&profile, `(deny file-read* (subpath (param %q)))`, key)
	}

	if !policy.network {
		profile.WriteString("(deny network*)")
	}

	args = append(args, "-p", profile.String())

	return append(args, argv...), nil
}
