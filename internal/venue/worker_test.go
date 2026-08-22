package venue

import "testing"

// TestParseWorkerForms pins the small grammar. It is small on purpose:
// anything describing the MACHINE rather than the connection belongs to
// whatever provisioned it, not to a pipeline runner dialing in.
func TestParseWorkerForms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		raw     string
		scheme  Scheme
		user    string
		host    string
		root    string
		wantErr bool
	}{
		{name: "local", raw: "local:", scheme: SchemeLocal},
		{name: "ssh with user", raw: "ssh://jt@box", scheme: SchemeSSH, user: "jt", host: "box"},
		{name: "ssh with port", raw: "ssh://box:2222", scheme: SchemeSSH, host: "box:2222"},
		{name: "ssh with root", raw: "ssh://jt@box/srv/steps", scheme: SchemeSSH, user: "jt", host: "box", root: "srv/steps"},
		{name: "ssh without host", raw: "ssh://", wantErr: true},
		{name: "local naming a host", raw: "local://box", wantErr: true},
		{name: "unknown scheme", raw: "http://box", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			worker, err := ParseWorker(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseWorker(%q) succeeded, want a refusal", tc.raw)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseWorker(%q): %v", tc.raw, err)
			}

			if worker.Scheme != tc.scheme || worker.User != tc.user || worker.Host != tc.host || worker.Root != tc.root {
				t.Errorf("ParseWorker(%q) = %+v, want scheme %q user %q host %q root %q",
					tc.raw, worker, tc.scheme, tc.user, tc.host, tc.root)
			}
		})
	}
}

// TestParseWorkerOptions pins the query options, which are how an operator
// says "this key", "these host keys", "that binary".
func TestParseWorkerOptions(t *testing.T) {
	t.Parallel()

	worker, err := ParseWorker("ssh://jt@box?identity=/k&known_hosts=/kh&binary=/b")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	if worker.Identity != "/k" || worker.KnownHosts != "/kh" || worker.Binary != "/b" {
		t.Errorf("options = %+v, want all three carried", worker)
	}
}
