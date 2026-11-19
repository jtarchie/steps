package pipeline

// on_pending: telling someone a run is waiting on them.

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
)

type pendingHook struct {
	scope hookScope
	step  *config.Step
}

type pendingHookKey struct{}

// withPendingHook makes step the on_pending a park under ctx announces through. Only the nearest fires, so a nil step keeps the enclosing one.
func withPendingHook(ctx context.Context, scope hookScope, step *config.Step) context.Context {
	if step == nil {
		return ctx
	}

	return context.WithValue(ctx, pendingHookKey{}, pendingHook{scope: scope, step: step})
}

// park is one request waiting on a person, as an on_pending reads it.
type park struct {
	id        int64
	kind      string
	message   string
	options   []string
	expiresAt time.Time
}

// fields are ((.:pending.*)), and STEPS_PENDING_* spelled as text.
func (p park) fields(ctx context.Context, cfg *config.Config, job string) (map[string]any, map[string]string) {
	section, command := "approvals", fmt.Sprintf("steps approvals approve %d %s", p.id, agent.AnswerFlags(ctx, cfg.Name))
	if p.kind != "approval" {
		section, command = "questions", fmt.Sprintf("steps questions answer %d <answer> %s", p.id, agent.AnswerFlags(ctx, cfg.Name))
	}

	link := ""
	if base := strings.TrimRight(shell.MetadataFrom(ctx).URL, "/"); base != "" {
		link = base + "/p/" + url.PathEscape(cfg.Name) + "/" + section + "?id=" + strconv.FormatInt(p.id, 10)
	}

	options := make([]any, len(p.options))
	for i, option := range p.options {
		options[i] = option
	}

	captured := map[string]any{
		"id":         strconv.FormatInt(p.id, 10),
		"kind":       p.kind,
		"job":        job,
		"message":    p.message,
		"options":    options,
		"command":    command,
		"url":        link,
		"expires_at": p.expiresAt.UTC().Format(time.RFC3339),
	}

	env := make(map[string]string, len(captured))

	// Spelled as ((.:pending.<field>)) renders it, so a command and a capture read the same text.
	for name := range captured {
		text, err := config.LocalValue(captured, config.LocalRef{Root: config.PendingCapture, Fields: []string{name}})
		if err == nil {
			env[config.PendingEnvName(name)] = text
		}
	}

	return captured, env
}

// announce runs the nearest on_pending for a park and reports whether there was one to attempt. Its failure is logged and noted on the run, never the wait's: the run keeps waiting either way.
func announce(ctx context.Context, p park) bool {
	hook, ok := ctx.Value(pendingHookKey{}).(pendingHook)
	if !ok {
		return false
	}

	captured, env := p.fields(ctx, hook.scope.cfg, hook.scope.jobName)

	hookCtx := withCellVars(ctx)
	runVarsFrom(hookCtx).set(config.PendingCapture, capture{value: captured})
	hookCtx = shell.WithPendingEnv(hookCtx, env)

	err := runMatchedHook(hookCtx, hook.scope, "on_pending", hook.step)
	if err != nil {
		logIfHookFailed(ctx, hook.scope, "on_pending", err)
		events.Note(ctx, events.NoteWarn, fmt.Sprintf("%s %d: on_pending failed, still waiting: %v", p.kind, p.id, err))
	}

	return true
}

// announceApproval announces an approval nobody has been told about yet: a resumed wait whose notice was already attempted stays quiet, and one decided while nobody watched has nothing to announce.
func announceApproval(ctx context.Context, st store.Approvals, id int64, message string, timeout time.Duration) {
	row, err := st.ApprovalStatus(ctx, id)
	if err != nil || row.Status != store.ApprovalPending || !row.NotifiedAt.IsZero() {
		return
	}

	if announce(ctx, park{id: id, kind: "approval", message: message, expiresAt: row.RequestedAt.Add(timeout)}) {
		bestEffort(ctx, "MarkApprovalNotified", st.MarkApprovalNotified(context.WithoutCancel(ctx), id))
	}
}

// announceQuestion is the agent package's ParkAnnouncer: an ask_user question parked mid-step.
func announceQuestion(ctx context.Context, row store.Question, expiresAt time.Time) bool {
	return announce(ctx, park{id: row.ID, kind: "ask_user", message: row.Question, options: row.Options, expiresAt: expiresAt})
}
