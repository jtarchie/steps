package redact

import "testing"

func TestCommandMasksURLCredentials(t *testing.T) {
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
		got := Command(in)
		if got != want {
			t.Errorf("Command(%q) = %q, want %q", in, got, want)
		}
	}
}
