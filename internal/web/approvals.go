package web

import (
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

// decisionList is the list an approvals or questions page draws over: the job and status chips, the box, and the cursor. status is the chip chosen, empty for all.
func (s *Server) decisionList(c *echo.Context, section, hintKey, labelKey string, statuses []string) (list *listView, job, status string, before int64) {
	pipeline := pipelineOf(c)
	loc := s.renderer.localeOf(c)
	query := c.Request().URL.Query()

	list = newList("/p/"+pipeline.Slug+"/"+section, query, loc.text(hintKey), loc.text(labelKey))
	job = list.facet(loc, "job", pipeline.Config().JobNames(), identity)
	status = list.facet(loc, "status", statuses, func(v string) string { return loc.word("status", v) })
	list.searched()

	before, _ = strconv.ParseInt(query.Get("before"), 10, 64)

	return list, job, status, before
}

// handleApprovals draws what still waits, oldest first and never capped — a build parked behind one that scrolled off would wait forever — above a paged history of what was decided.
func (s *Server) handleApprovals(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	decided := []store.ApprovalStatus{store.ApprovalApproved, store.ApprovalRejected, store.ApprovalExpired}
	list, job, status, before := s.decisionList(c, "approvals", "approvals.search.hint", "approvals.search.label", []string{string(store.ApprovalPending), string(store.ApprovalApproved), string(store.ApprovalRejected), string(store.ApprovalExpired)})

	var pending, history []store.Approval

	// Only on the first page: a reader paging back through the history has already been shown what waits.
	if before == 0 && (status == "" || status == string(store.ApprovalPending)) {
		var err error

		pending, err = pipeline.Store.FindApprovals(ctx, store.ApprovalQuery{Job: job, Text: list.Text, Statuses: []store.ApprovalStatus{store.ApprovalPending}})
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		slices.Reverse(pending)
	}

	if status != string(store.ApprovalPending) {
		if status != "" {
			decided = []store.ApprovalStatus{store.ApprovalStatus(status)}
		}

		var err error

		history, err = pipeline.Store.FindApprovals(ctx, store.ApprovalQuery{Job: job, Text: list.Text, Statuses: decided, Before: before, Limit: decisionsPage + 1})
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		history = page(list, history, decisionsPage, func(row store.Approval) string { return strconv.FormatInt(row.ID, 10) })
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "approvals", map[string]any{
		"Nav":     s.nav(c),
		"Title":   "approvals",
		"Pending": pending,
		"History": history,
		"List":    list,
	})
}

// handleQuestions is handleApprovals for what agents asked.
func (s *Server) handleQuestions(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	closed := []store.QuestionStatus{store.QuestionAnswered, store.QuestionExpired, store.QuestionAborted}
	list, job, status, before := s.decisionList(c, "questions", "questions.search.hint", "questions.search.label", []string{string(store.QuestionPending), string(store.QuestionAnswered), string(store.QuestionExpired), string(store.QuestionAborted)})

	var pending, history []store.Question

	if before == 0 && (status == "" || status == string(store.QuestionPending)) {
		var err error

		pending, err = pipeline.Store.FindQuestions(ctx, store.QuestionQuery{Job: job, Text: list.Text, Statuses: []store.QuestionStatus{store.QuestionPending}})
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		slices.Reverse(pending)
	}

	if status != string(store.QuestionPending) {
		if status != "" {
			closed = []store.QuestionStatus{store.QuestionStatus(status)}
		}

		var err error

		history, err = pipeline.Store.FindQuestions(ctx, store.QuestionQuery{Job: job, Text: list.Text, Statuses: closed, Before: before, Limit: decisionsPage + 1})
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		history = page(list, history, decisionsPage, func(row store.Question) string { return strconv.FormatInt(row.ID, 10) })
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "questions", map[string]any{
		"Nav":     s.nav(c),
		"Title":   "questions",
		"Pending": pending,
		"History": history,
		"List":    list,
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
