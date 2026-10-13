package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// handleApprovals lists decisions, pending first.
func (s *Server) handleApprovals(c *echo.Context) error {
	pipeline := pipelineOf(c)

	approvals, err := pipeline.Store.Approvals(c.Request().Context(), false, historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "approvals", map[string]any{
		"Nav":       s.nav(c),
		"Title":     "approvals",
		"Approvals": approvals,
	})
}

// handleQuestions lists what agents have asked: everything still waiting
// first, then the rest newest-first (see Store.Questions for why the order is not
// simply recency).
func (s *Server) handleQuestions(c *echo.Context) error {
	pipeline := pipelineOf(c)

	questions, err := pipeline.Store.Questions(c.Request().Context(), false, historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "questions", map[string]any{
		"Nav":       s.nav(c),
		"Title":     "questions",
		"Questions": questions,
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

	status := "approved"
	if c.FormValue("decision") == "reject" {
		status = "rejected"
	}

	// A decision that no longer matches a pending row is a resubmission — a
	// double click, or a refresh re-POSTing the form. The first one recorded
	// the audit trail correctly, so settle on it rather than reporting a 500
	// for work that succeeded. Any other failure recorded nothing, and a
	// redirect would tell the person their decision landed while the build
	// stays parked on it.
	err = pipeline.Store.DecideApproval(c.Request().Context(), id, status, "web", c.FormValue("reason"))

	switch {
	case err == nil:
	case errors.Is(err, store.ErrApprovalNotPending):
		slog.Info("web.approval_not_pending", "id", id, "error", err)
	default:
		slog.Error("web.approval_decide_failed", "id", id, "error", err)

		return echo.NewHTTPError(http.StatusInternalServerError, "could not record the decision").Wrap(err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/approvals")
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

	// Two failures, and only one of them is the caller's business. A question
	// somebody else just answered (or that expired while this form sat open) is
	// a resubmission, exactly like a double-clicked approval: the row is the
	// answer of record and it already has one, so settle on it.
	//
	// A REFUSED answer is different and must be shown. Redirecting on an
	// options-fence rejection left the question listed as pending with no
	// message anywhere, so the person retypes the same thing or concludes the
	// step is stuck — while the agent is still waiting on them.
	err = pipeline.Store.AnswerQuestion(c.Request().Context(), id, answer, "web")

	switch {
	case err == nil:
	case errors.Is(err, store.ErrQuestionNotPending):
		slog.Info("web.question_already_resolved", "id", id, "error", err)
	default:
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/questions")
}
