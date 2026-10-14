package shell

import (
	"strings"
	"testing"
)

func TestExitErrorDoesNotCarryURLCredentials(t *testing.T) {
	t.Parallel()

	const command = "git push https://u:secret@example.com/r.git" //nolint:gosec // a fake credential, the input under test

	for _, err := range []*ExitError{
		{Command: command, Code: 1},
		{Command: command, Venue: "ssh://w", Code: 1},
	} {
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("Error() = %q, leaks the password", err.Error())
		}

		if err.Command != command {
			t.Errorf("Command = %q, want it kept intact for whoever runs it", err.Command)
		}
	}
}
