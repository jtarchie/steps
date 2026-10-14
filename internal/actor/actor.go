// Package actor names who or what caused something steps records: a person,
// or the automatic thing that stood in for one. It is a stdlib-only leaf so
// the request that learns the person and the page that shows them can share
// one vocabulary without either importing the other.
//
// The serialized form ("kind:id") is what the state database stores, so the
// store stays a string column and never learns this type.
package actor

import (
	"context"
	"strings"
)

// Actor is a kind and an id within it. The zero value is "nobody recorded",
// which every reader treats as absent rather than as a name.
type Actor struct {
	Kind string
	ID   string
}

const (
	kindLocal    = "local"
	kindOS       = "os"
	kindCheck    = "check"
	kindWebhook  = "webhook"
	kindUpstream = "upstream"
	kindBacklog  = "backlog"
)

// Local is a person who signed in with one of the daemon's own accounts.
func Local(name string) Actor { return Actor{Kind: kindLocal, ID: name} }

// OS is a person named only by the environment of a command run on the
// daemon's machine. Nothing verified it, and every rendering says so.
func OS(user string) Actor { return Actor{Kind: kindOS, ID: user} }

// Check is a new version of a resource starting work.
func Check(resource string) Actor { return Actor{Kind: kindCheck, ID: resource} }

// Webhook is a delivery to a webhook resource starting work.
func Webhook(resource string) Actor { return Actor{Kind: kindWebhook, ID: resource} }

// Upstream is a passed: constraint clearing once upstream jobs went green.
func Upstream() Actor { return Actor{Kind: kindUpstream} }

// Backlog is a job queueing itself again for versions a build left waiting.
func Backlog() Actor { return Actor{Kind: kindBacklog} }

// IsZero reports an actor nobody recorded.
func (a Actor) IsZero() bool { return a.Kind == "" && a.ID == "" }

// String is the stored form. A kind with no id still keeps its colon, so
// Parse cannot mistake it for a legacy bare name.
func (a Actor) String() string {
	if a.IsZero() {
		return ""
	}

	return a.Kind + ":" + a.ID
}

// Parse reads the stored form back. A value with no colon is a name written
// before actors existed (approvals recorded "web"), kept as an id with no
// kind rather than guessed at.
func Parse(stored string) Actor {
	kind, id, found := strings.Cut(stored, ":")
	if !found {
		return Actor{ID: stored}
	}

	return Actor{Kind: kind, ID: id}
}

// Unverified reports an actor whose name nobody checked.
func (a Actor) Unverified() bool { return a.Kind == kindOS }

// Automatic reports an actor that is not a person: a check, a delivery, a
// passed: constraint or a backlog.
func (a Actor) Automatic() bool {
	switch a.Kind {
	case kindCheck, kindWebhook, kindUpstream, kindBacklog:
		return true
	default:
		return false
	}
}

// Label is how a reader sees the actor: a person by name and where the name
// came from, anything else as what it is.
func (a Actor) Label() string {
	switch a.Kind {
	case kindOS:
		return a.ID + " (this machine, unverified)"
	case kindCheck:
		return "new version of " + a.ID
	case kindWebhook:
		return "webhook: " + a.ID
	case kindUpstream:
		return "upstream jobs passed"
	case kindBacklog:
		return "more versions to build"
	case "":
		return a.ID
	default:
		return a.ID + " (" + a.Kind + ")"
	}
}

type contextKey struct{}

// With carries the actor a request was made by into everything it calls.
func With(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, contextKey{}, a)
}

// From is the actor ctx carries, zero when none was set.
func From(ctx context.Context) Actor {
	a, _ := ctx.Value(contextKey{}).(Actor)

	return a
}
