package shell

import (
	"strings"
	"testing"
)

func TestRedactCommandMasksURLCredentials(t *testing.T) {
	t.Parallel()

	cases := map[string]string{ //nolint:gosec // fake credentials, the input under test
		"git clone https://u:secret@example.com/r.git":         "git clone https://u:***@example.com/r.git",
		"curl https://ghp_token@api.github.com/x":              "curl https://***@api.github.com/x",
		"psql 'postgres://admin:p4ss@db:5432/app' -c 'select'": "psql 'postgres://admin:***@db:5432/app' -c 'select'",
		"a https://x:1@h b git+ssh://y:2@g":                    "a https://x:***@h b git+ssh://y:***@g",
		"curl https://example.com:8443/path?q=a@b":             "curl https://example.com:8443/path?q=a@b",
		"echo plain words, no url":                             "echo plain words, no url",
		"mail user@example.com":                                "mail user@example.com",
	}

	for in, want := range cases {
		got := RedactCommand(in)
		if got != want {
			t.Errorf("RedactCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

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
