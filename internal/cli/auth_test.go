package cli

// The CLI's half of basic auth: credentials come out of the --target URL and never go back into anything a person or a log reads.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTheTargetsCredentialsTravelInTheHeaderAndNotTheURL(t *testing.T) {
	t.Parallel()

	var seen struct {
		user, pass string
		ok         bool
		path       string
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.user, seen.pass, seen.ok = r.BasicAuth()
		seen.path = r.URL.String()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	withCreds := strings.Replace(server.URL, "http://", "http://ops:s3cret@", 1)

	client := newDaemonClient(withCreds)

	_, _, err := client.get("demo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if !seen.ok || seen.user != "ops" || seen.pass != "s3cret" {
		t.Errorf("the daemon saw basic auth %q/%q (ok=%v), want ops/s3cret", seen.user, seen.pass, seen.ok)
	}

	if strings.Contains(seen.path, "ops") || strings.Contains(seen.path, "s3cret") {
		t.Errorf("the request URL carried the credentials: %q", seen.path)
	}

	// A redirect, a proxy log and every message below all read this field, so the userinfo must be off it.
	if strings.Contains(client.target, "ops") || strings.Contains(client.target, "s3cret") {
		t.Errorf("the client's target still carries the credentials: %q", client.target)
	}
}

// A target with no userinfo must send no Authorization at all: an empty credential pair offered to an open daemon is a 401 from a server configured to accept one.
func TestATargetWithoutCredentialsSendsNone(t *testing.T) {
	t.Parallel()

	offered := true

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, offered = r.BasicAuth()

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, _, err := newDaemonClient(server.URL).get("demo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if offered {
		t.Error("a target with no userinfo still sent an Authorization header")
	}
}

// Every message this client writes names its target, and a password that reached one would reach a terminal, a CI log and whatever bug report quotes it.
func TestNoMessageCarriesThePasswordOutOfTheTarget(t *testing.T) {
	t.Parallel()

	const password = "p4ssw0rd-do-not-print"

	refuse := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}

	for name, server := range map[string]*httptest.Server{
		"unauthorized": refuse(http.StatusUnauthorized, "Unauthorized"),
		"teapot":       refuse(http.StatusTeapot, `{"message":"no"}`),
	} {
		defer server.Close()

		target := strings.Replace(server.URL, "http://", "http://ops:"+password+"@", 1)

		_, _, err := newDaemonClient(target).get("demo")
		if err == nil {
			t.Fatalf("%s: a refusal was read as an answer", name)
		}

		if strings.Contains(err.Error(), password) {
			t.Errorf("%s: the refusal printed the password: %v", name, err)
		}
	}

	// Unreachable is the other shape, and its message quotes the target twice. A closed server's address rather than a low port, which on a developer's machine may well be answered by something.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	target := strings.Replace(dead.URL, "http://", "http://ops:"+password+"@", 1)

	_, _, err := newDaemonClient(target).get("demo")
	if err == nil {
		t.Fatal("a closed port was read as an answer")
	}

	if strings.Contains(err.Error(), password) {
		t.Errorf("the unreachable message printed the password: %v", err)
	}
}

// echo's own 401 body is "Unauthorized", which names neither what is missing nor where it goes.
func TestA401NamesTheFix(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `basic realm="steps"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}))
	defer server.Close()

	_, _, err := newDaemonClient(server.URL).get("demo")
	if err == nil {
		t.Fatal("a 401 was read as an answer")
	}

	for _, want := range []string{"credentials", "--target http://user:password@host", "STEPS_TARGET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the 401 message does not mention %q: %v", want, err)
		}
	}
}

func TestSplitTargetCredentials(t *testing.T) {
	t.Parallel()

	for _, probe := range []struct {
		target, base, user, pass string
	}{
		{"http://127.0.0.1:8088", "http://127.0.0.1:8088", "", ""},
		{"http://127.0.0.1:8088/", "http://127.0.0.1:8088", "", ""},
		{"http://ops:s3cret@host:8088", "http://host:8088", "ops", "s3cret"},
		{"https://ops:s3cret@steps.example.com/", "https://steps.example.com", "ops", "s3cret"},
		// A username with no password is what a person who put the password elsewhere types, and it must not become a password.
		{"http://ops@host:8088", "http://host:8088", "ops", ""},
		// Percent-encoding is how a password holding a @ or a : reaches a URL at all.
		{"http://ops:p%40ss%3Aword@host", "http://host", "ops", "p@ss:word"},
	} {
		base, user, pass := splitTargetCredentials(probe.target)
		if base != probe.base || user != probe.user || pass != probe.pass {
			t.Errorf("splitTargetCredentials(%q) = %q %q %q, want %q %q %q",
				probe.target, base, user, pass, probe.base, probe.user, probe.pass)
		}
	}
}

func TestLoopbackOnly(t *testing.T) {
	t.Parallel()

	for listen, want := range map[string]bool{
		"127.0.0.1:8088": true,
		"127.0.0.2:8088": true,
		"[::1]:8088":     true,
		"localhost:8088": true,
		"0.0.0.0:8088":   false,
		"[::]:8088":      false,
		":8088":          false,
		"fly-local:8088": false,
		"10.0.0.5:8088":  false,
		"nonsense":       false,
	} {
		if got := loopbackOnly(listen); got != want {
			t.Errorf("loopbackOnly(%q) = %v, want %v", listen, got, want)
		}
	}
}

// A port NAME nothing resolves, so the listen fails after the banner and the warning without ever serving. A low port is not that: macOS lets an ordinary user bind port 1, which serves until the suite times out and takes the address other tests treat as dead.
func unservable(host string) string { return host + ":not-a-port" }

// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestAnUnauthenticatedPublicListenWarnsOnce(t *testing.T) {
	out := captureStdout(t, func() {
		_ = (&WebCmd{Listen: unservable("0.0.0.0"), Interval: time.Second, DB: DB(filepath.Join(t.TempDir(), "steps.db"))}).Run()
	})

	if strings.Count(out, "WARNING") != 1 {
		t.Errorf("a public listen with no credentials did not warn exactly once:\n%s", out)
	}

	if !strings.Contains(out, "--basic-auth-username") {
		t.Errorf("the warning does not name the flags that fix it:\n%s", out)
	}
}

// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestALoopbackListenAndAnAuthedPublicOneAreQuiet(t *testing.T) {
	for _, cmd := range []*WebCmd{
		{Listen: unservable("127.0.0.1"), Interval: time.Second, DB: DB(filepath.Join(t.TempDir(), "steps.db"))},
		{Listen: unservable("0.0.0.0"), Interval: time.Second, DB: DB(filepath.Join(t.TempDir(), "steps.db")), BasicAuthUsername: "ops", BasicAuthPassword: "s3cret"},
	} {
		out := captureStdout(t, func() { _ = cmd.Run() })

		if strings.Contains(out, "WARNING") {
			t.Errorf("%s warned:\n%s", cmd.Listen, out)
		}

		if strings.Contains(out, "s3cret") {
			t.Errorf("%s printed the password:\n%s", cmd.Listen, out)
		}
	}
}
