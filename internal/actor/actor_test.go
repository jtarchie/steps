package actor

import (
	"context"
	"testing"
)

func TestTheStoredFormRoundTrips(t *testing.T) {
	t.Parallel()

	for _, a := range []Actor{Local("ops"), OS("jt"), Check("repo"), Webhook("push"), Upstream(), Backlog(), {Kind: "github", ID: "alex"}} {
		if got := Parse(a.String()); got != a {
			t.Errorf("Parse(%q) = %#v, want %#v", a.String(), got, a)
		}
	}
}

func TestABareNameIsKeptRatherThanGuessedAt(t *testing.T) {
	t.Parallel()

	got := Parse("web")
	if got != (Actor{ID: "web"}) || got.Label() != "web" {
		t.Errorf("Parse(%q) = %#v labelled %q, want the bare name kept", "web", got, got.Label())
	}
}

func TestLabels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		actor Actor
		want  string
	}{
		{Local("ops"), "ops (local)"},
		{Actor{Kind: "github", ID: "alex"}, "alex (github)"},
		{OS("jt"), "jt (this machine, unverified)"},
		{Check("tick"), "new version of tick"},
		{Webhook("push"), "webhook: push"},
		{Upstream(), "upstream jobs passed"},
		{Backlog(), "more versions to build"},
	} {
		if got := tc.actor.Label(); got != tc.want {
			t.Errorf("%#v.Label() = %q, want %q", tc.actor, got, tc.want)
		}
	}
}

func TestOnlyAnEnvironmentNameIsUnverified(t *testing.T) {
	t.Parallel()

	if !OS("jt").Unverified() || Local("jt").Unverified() || Check("r").Unverified() {
		t.Error("Unverified must be true for an OS name and false for everything else")
	}
}

func TestOnlyANonPersonIsAutomatic(t *testing.T) {
	t.Parallel()

	for _, a := range []Actor{Check("r"), Webhook("h"), Upstream(), Backlog()} {
		if !a.Automatic() {
			t.Errorf("%#v is not automatic", a)
		}
	}

	for _, a := range []Actor{Local("ops"), OS("jt"), {Kind: "github", ID: "alex"}, Parse("web")} {
		if a.Automatic() {
			t.Errorf("%#v is automatic", a)
		}
	}
}

func TestNobodyRecordedIsEmpty(t *testing.T) {
	t.Parallel()

	if (Actor{}).String() != "" || !From(context.Background()).IsZero() {
		t.Error("the zero actor must store as empty and be what an unmarked context carries")
	}

	if got := From(With(context.Background(), Local("ops"))); got != Local("ops") {
		t.Errorf("From(With(ops)) = %#v", got)
	}
}
