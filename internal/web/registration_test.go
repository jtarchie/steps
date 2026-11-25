package web

import (
	"testing"
	"time"
)

func TestRegistrationsAreCountedPerSourceAndForget(t *testing.T) {
	t.Parallel()

	if a, b := registrationSource("[2001:db8:1:2::1]:443"), registrationSource("[2001:db8:1:2:ffff::9]:80"); a != b {
		t.Errorf("two addresses of one /64 are sources %q and %q, want one", a, b)
	}

	if a, b := registrationSource("192.0.2.1:1"), registrationSource("192.0.2.2:1"); a == b {
		t.Errorf("two IPv4 hosts share source %q", a)
	}

	var log registrationLog

	start := time.Now()
	for i := range registrationsPerSource {
		if !log.allow("a", start) {
			t.Fatalf("registration %d of the allowance was refused", i+1)
		}
	}

	if log.allow("a", start) {
		t.Error("a registration past the allowance was accepted")
	}

	if !log.allow("b", start) {
		t.Error("another source was refused for the first one's registrations")
	}

	if !log.allow("a", start.Add(registrationWindow)) {
		t.Error("a source was still refused once its window had passed")
	}
}
