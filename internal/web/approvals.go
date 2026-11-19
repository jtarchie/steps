package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// decisionsPage is how many decided rows one page of the approvals or questions history shows.
const decisionsPage = 50

// decisionKind is all that differs between the approvals page and the questions page: the words, the statuses, and the one query each store answers. Everything else — the chips, the box, what waits above what was decided, the cursor — is decisionPage, written once.
type decisionKind[T any] struct {
	section, hint, label string
	// pending is the status of a row still waiting on a person; closed are the others.
	pending string
	closed  []string
	find    func(ctx context.Context, id int64, job, text string, statuses []string, before int64, limit int) ([]T, error)
	id      func(T) int64
}

// typed is a list of status words as the store's own status type.
func typed[S ~string](words []string) []S {
	out := make([]S, len(words))
	for i, word := range words {
		out[i] = S(word)
	}

	return out
}

// decisionPage draws what still waits, oldest first and never capped — a build parked behind one that scrolled off would wait forever — above a paged history of what was decided.
func decisionPage[T any](s *Server, c *echo.Context, kind decisionKind[T]) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	loc := s.renderer.localeOf(c)
	query := c.Request().URL.Query()

	list := newList("/p/"+pipeline.Slug+"/"+kind.section, query, loc.text(kind.hint), loc.text(kind.label))
	job := list.facet(loc, "job", pipeline.Config().JobNames(), identity)
	status := list.facet(loc, "status", append([]string{kind.pending}, kind.closed...), func(v string) string { return loc.word("status", v) })
	// The one row an on_pending notice links to; a chip of its own so the reader can widen back out.
	id, _ := strconv.ParseInt(list.facet(loc, "id", nil, identity), 10, 64)
	list.searched()

	before, _ := strconv.ParseInt(query.Get("before"), 10, 64)

	var pending, history []T

	// Only on the first page: a reader paging back through the history has already been shown what waits.
	if before == 0 && (status == "" || status == kind.pending) {
		var err error

		pending, err = kind.find(ctx, id, job, list.Text, []string{kind.pending}, 0, 0)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		slices.Reverse(pending)
	}

	if status != kind.pending {
		decided := kind.closed
		if status != "" {
			decided = []string{status}
		}

		var err error

		history, err = kind.find(ctx, id, job, list.Text, decided, before, decisionsPage+1)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		history = page(list, history, decisionsPage, func(row T) string { return strconv.FormatInt(kind.id(row), 10) })
	}

	list.Shown = len(pending) + len(history)

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, kind.section, map[string]any{
		"Nav":     s.nav(c),
		"Title":   kind.section,
		"Pending": pending,
		"History": history,
		"List":    list,
	})
}

func (s *Server) handleApprovals(c *echo.Context) error {
	pipeline := pipelineOf(c)

	return decisionPage(s, c, decisionKind[store.Approval]{
		section: "approvals", hint: "approvals.search.hint", label: "approvals.search.label",
		pending: string(store.ApprovalPending),
		closed:  []string{string(store.ApprovalApproved), string(store.ApprovalRejected), string(store.ApprovalExpired)},
		find: func(ctx context.Context, id int64, job, text string, statuses []string, before int64, limit int) ([]store.Approval, error) {
			return pipeline.Store.FindApprovals(ctx, store.ApprovalQuery{ID: id, Job: job, Text: text, Statuses: typed[store.ApprovalStatus](statuses), Before: before, Limit: limit})
		},
		id: func(a store.Approval) int64 { return a.ID },
	})
}

func (s *Server) handleQuestions(c *echo.Context) error {
	pipeline := pipelineOf(c)

	return decisionPage(s, c, decisionKind[store.Question]{
		section: "questions", hint: "questions.search.hint", label: "questions.search.label",
		pending: string(store.QuestionPending),
		closed:  []string{string(store.QuestionAnswered), string(store.QuestionExpired), string(store.QuestionAborted)},
		find: func(ctx context.Context, id int64, job, text string, statuses []string, before int64, limit int) ([]store.Question, error) {
			return pipeline.Store.FindQuestions(ctx, store.QuestionQuery{ID: id, Job: job, Text: text, Statuses: typed[store.QuestionStatus](statuses), Before: before, Limit: limit})
		},
		id: func(q store.Question) int64 { return q.ID },
	})
}

// handleDecideApproval records a human decision, through the same row the
// CLI's approve/reject write.
func (s *Server) handleDecideApproval(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	pipeline := pipelineOf(c)

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "approval id must be a number")
	}

	var status store.ApprovalStatus

	switch c.FormValue("decision") {
	case "approve":
		status = store.ApprovalApproved
	case "reject":
		status = store.ApprovalRejected
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "decision must be approve or reject")
	}

	reason := c.FormValue("reason")
	if len(reason) > store.MaxFreeTextBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "the reason is too long")
	}

	// A decision that no longer matches a pending row is a resubmission — a
	// double click, or a refresh re-POSTing the form. The first one recorded
	// the audit trail correctly, so settle on it rather than reporting a 500
	// for work that succeeded. Any other failure recorded nothing, and a
	// redirect would tell the person their decision landed while the build
	// stays parked on it.
	err = pipeline.Store.DecideApproval(c.Request().Context(), id, status, decidedBy(c), reason)

	switch {
	case err == nil:
	case errors.Is(err, store.ErrApprovalNotPending):
		slog.Info("web.approval_not_pending", "id", id, "error", err)
	default:
		slog.Error("web.approval_decide_failed", "id", id, "error", err)

		return echo.NewHTTPError(http.StatusInternalServerError, "could not record the decision").Wrap(err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, returnTo(c, "/p/"+pipeline.Slug+"/approvals"))
}

// handleAnswerQuestion records an answer, through the same row `steps questions answer`
// writes — including its options fence, which lives in the store precisely so
// that this handler cannot be the place it is forgotten.
func (s *Server) handleAnswerQuestion(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	pipeline := pipelineOf(c)

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "question id must be a number")
	}

	// An option button wins over the free-text field, which is what somebody
	// who clicked one meant even if they had typed something first.
	answer := strings.TrimSpace(c.FormValue("answer"))
	if answer == "" {
		answer = strings.TrimSpace(c.FormValue("answer_text"))
	}

	if answer == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "an answer is required")
	}

	if len(answer) > store.MaxFreeTextBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "the answer is too long")
	}

	// Two failures, and only one of them is the caller's business. A question
	// somebody else just answered (or that expired while this form sat open) is
	// a resubmission, exactly like a double-clicked approval: the row is the
	// answer of record and it already has one, so settle on it.
	//
	// A REFUSED answer is different and must be shown. Redirecting on an
	// options-fence rejection left the question listed as pending with no
	// message anywhere, so the person retypes the same thing or concludes the
	// step is stuck — while the agent is still waiting on them.
	err = pipeline.Store.AnswerQuestion(c.Request().Context(), id, answer, decidedBy(c))

	switch {
	case err == nil:
	case errors.Is(err, store.ErrQuestionNotPending):
		slog.Info("web.question_already_resolved", "id", id, "error", err)
	default:
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, returnTo(c, "/p/"+pipeline.Slug+"/questions"))
}

// decidedBy is who a decision made in the browser is recorded under: the signed-in person, or "web" on a server nobody signs in to, which is all that is known there.
func decidedBy(c *echo.Context) string {
	by := actor.From(c.Request().Context()).String()
	if by == "" {
		return "web"
	}

	return by
}
