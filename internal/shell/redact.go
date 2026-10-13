package shell

import "regexp"

// urlUserinfo finds a URL's userinfo anywhere in a command line. A rendered
// command is shell, not a URL, so this scans for `scheme://...@` rather than
// parsing; it stops at whitespace, quotes and the path's first slash, so an
// @ later in a path or query is left alone.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)([^\s/@:'"]*)(:[^\s/@'"]*)?@`)

// RedactCommand masks the credentials a URL in command carries, for any copy
// of a command that leaves the process as text: an error, a log line, a run
// event. A source: is routinely rendered into a command as
// https://user:token@host, and errors are persisted and shown in the browser.
// The command that is EXECUTED is never redacted.
func RedactCommand(command string) string {
	return urlUserinfo.ReplaceAllStringFunc(command, func(match string) string {
		parts := urlUserinfo.FindStringSubmatch(match)
		if parts[3] == "" {
			return parts[1] + "***@"
		}

		return parts[1] + parts[2] + ":***@"
	})
}
