package cli

import "testing"

func TestExternalURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		listen, flag, want string
		wantErr            bool
	}{
		{listen: "127.0.0.1:8088", want: "http://127.0.0.1:8088"},
		{listen: "[::1]:8088", want: "http://[::1]:8088"},
		{listen: "0.0.0.0:8088"},
		{listen: ":8088"},
		{listen: "[::]:8088"},
		{listen: "0.0.0.0:8088", flag: "https://ci.example/steps/", want: "https://ci.example/steps"},
		{listen: "127.0.0.1:1", flag: "ftp://x", wantErr: true},
		{listen: "127.0.0.1:1", flag: "not a url", wantErr: true},
		{listen: "127.0.0.1:1", flag: "https://u:p@h", wantErr: true}, //nolint:gosec // a refused URL, not a credential
		{listen: "127.0.0.1:1", flag: "https://h?x=1", wantErr: true},
		{listen: "127.0.0.1:1", flag: "https://h#f", wantErr: true},
	}

	for _, tc := range cases {
		got, err := externalURL(tc.listen, tc.flag)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("externalURL(%q, %q) = %q, %v; want %q, err=%v", tc.listen, tc.flag, got, err, tc.want, tc.wantErr)
		}
	}
}
