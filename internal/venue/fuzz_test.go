package venue

import (
	"net"
	"slices"
	"strings"
	"testing"
)

// FuzzParseWorker holds the --worker grammar to its promises: an accepted mapping names a known scheme, reads back identically, keeps its credentials out of the address a run record shows, and — on an acquisition rung — rebuilds into a URL that names the acquired instance and parses back to the same connection.
func FuzzParseWorker(f *testing.F) {
	for _, seed := range []string{
		"local:",
		"local:/mnt/fast",
		"ssh://ubuntu@box:2222/mnt/fast?identity=/k&known_hosts=/h",
		"ssh://box?hostkey=SHA256:" + strings.Repeat("a", 43),
		"aws://i-0abc123def456789/scratch?region=us-east-2",
		"aws://stopped/i-0abc123def456789?idle=5m",
		"aws://launch/lt-0def456789abcdef/root?version=latest&capacity=spot",
		"gcp://launch/template-1/work?zone=us-central1-a&project=p&idle=1m",
		"gcp://stopped/worker-1?hostkey=SHA256:" + strings.Repeat("B", 43),
		"aws://launch/lt-0def456789abcdef/a%3Fb",
		"ssh://box/%zz",
		"docker+ssh://jt@box:2222?sock=/run/docker.sock&identity=/k&hostkey=SHA256:" + strings.Repeat("c", 43),
		"aws://launch/lt-0def456789abcdef?capac%zz=spot",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		worker, err := ParseWorker(raw)
		if err != nil {
			return
		}

		if !slices.Contains([]Scheme{SchemeLocal, SchemeSSH, SchemeAWS, SchemeGCP, SchemeDockerSSH}, worker.Scheme) {
			t.Fatalf("ParseWorker(%q) accepted scheme %q", raw, worker.Scheme)
		}

		again, err := ParseWorker(worker.URL)
		if err != nil || again != worker {
			t.Fatalf("ParseWorker(%q) does not read back from its own URL: %+v vs %+v (%v)", raw, worker, again, err)
		}

		bare := worker
		bare.Identity, bare.KnownHosts, bare.SSHConfig, bare.HostKey, bare.Socket, bare.Query = "", "", "", "", "", ""

		if worker.Address() != bare.Address() {
			t.Fatalf("Address() of %q = %q depends on its connection options", raw, worker.Address())
		}

		if worker.needsAcquisition() {
			checkAcquiredURL(t, raw, worker)
		}
	})
}

// checkAcquiredURL holds asStatic to rebuilding a URL that parses back to the acquired instance with every connection option intact.
func checkAcquiredURL(t *testing.T, raw string, worker Worker) {
	t.Helper()

	instance := "i-0123456789abcdef0"
	if worker.Scheme == SchemeGCP {
		instance = "worker-acquired-1"
	}

	acquired := worker.asStatic(instance)

	static, err := ParseWorker(acquired.URL)
	if err != nil {
		t.Fatalf("%q acquired as %q, which does not parse: %v", raw, acquired.URL, err)
	}

	want := worker
	want.URL, want.Rung, want.Instance, want.Template = static.URL, RungStatic, instance, ""
	want.Query, want.Capacity, want.Version, want.Idle = static.Query, "", "$Default", defaultIdle

	if static != want {
		t.Fatalf("%q acquired as %q, which reads as\n%+v\nwant\n%+v", raw, acquired.URL, static, want)
	}
}

// FuzzSplitDirective pins the ssh_config line reader: the keyword is one lowercased token, and neither half carries a comment or the separator.
func FuzzSplitDirective(f *testing.F) {
	for _, seed := range []string{"Host box", "HostName=box.internal", "  IdentityFile ~/.ssh/id # key", "ProxyJump\t= jump", "#", ""} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		keyword, value := splitDirective(line)

		if keyword != strings.ToLower(keyword) || strings.ContainsAny(keyword, " \t=#") {
			t.Fatalf("splitDirective(%q) keyword = %q", line, keyword)
		}

		if strings.Contains(value, "#") || value != strings.Trim(value, " \t=") {
			t.Fatalf("splitDirective(%q) value = %q", line, value)
		}
	})
}

// FuzzExpandTokens pins that a string with no token is left alone, and that %% always yields a literal percent rather than starting another token.
func FuzzExpandTokens(f *testing.F) {
	f.Add("%h.internal", "box")
	f.Add("%%h", "box")
	f.Add("plain", "%h")
	f.Add("~/keys/%h", "a")

	f.Fuzz(func(t *testing.T, raw, alias string) {
		if !strings.Contains(raw, "%") {
			if got := expandHostname(raw, alias); got != raw {
				t.Fatalf("expandHostname(%q, %q) = %q", raw, alias, got)
			}

			if !strings.HasPrefix(raw, "~/") {
				if got := expandPath(raw, alias, "22", "u"); got != raw {
					t.Fatalf("expandPath(%q) = %q", raw, got)
				}
			}
		}

		if got := expandHostname("%%"+raw, alias); !strings.HasPrefix(got, "%") || got[1:] != expandHostname(raw, alias) {
			t.Fatalf("expandHostname(%%%%%q, %q) = %q", raw, alias, got)
		}
	})
}

// FuzzSplitHostPort pins that a split port joins back to its host, and that a portless authority loses at most its brackets.
func FuzzSplitHostPort(f *testing.F) {
	for _, seed := range []string{"box", "box:22", "[::1]", "[::1]:2222", "10.0.0.1:22", ":22", "a:b:c"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, authority string) {
		host, port := splitHostPort(authority)

		_, _, err := net.SplitHostPort(authority)
		if err != nil && !slices.Contains([]string{host, "[" + host + "]", "[" + host, host + "]"}, authority) {
			t.Fatalf("splitHostPort(%q) = %q, which is more than the brackets taken off", authority, host)
		}

		if port == "" {
			return
		}

		h, p, err := net.SplitHostPort(net.JoinHostPort(host, port))
		if err != nil || h != host || p != port {
			t.Fatalf("splitHostPort(%q) = %q, %q, which do not re-join", authority, host, port)
		}
	})
}
