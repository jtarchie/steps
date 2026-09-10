package web

// The endpoints `steps pipeline` talks to: outside /p/ because a set may CREATE the pipeline it names, and HTTP because a set needs a synchronous answer from the machine that will run it.

// No authentication, deliberately: a pipeline is arbitrary commands, so anyone who reaches this port runs anything as this user — which is why it binds loopback (docs/web.md).

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// PipelineSummary is one row of `steps pipeline list`. Exported and shared with the client for the reason SetRequest is: two copies of a wire struct drift into a silently-dropped field rather than a build error.
type PipelineSummary struct {
	Name   string `json:"name"`
	SHA    string `json:"sha"`
	From   string `json:"from,omitempty"`
	Jobs   int    `json:"jobs"`
	Paused bool   `json:"paused"`
}

// PipelineConfig is what get prints and what set diffs against: the source, its includes, and the sha that names both.
type PipelineConfig struct {
	Name     string            `json:"name"`
	SHA      string            `json:"sha"`
	Source   string            `json:"source"`
	Includes map[string]string `json:"includes,omitempty"`
	From     string            `json:"from,omitempty"`
	Paused   bool              `json:"paused"`
}

// handleAPIList answers what this daemon holds.
func (s *Server) handleAPIList(c echo.Context) error {
	served := s.Served()
	rows := make([]PipelineSummary, 0, len(served))

	for _, target := range served {
		row := PipelineSummary{
			Name:   target.Slug,
			From:   target.Path(),
			Jobs:   len(target.Config().Jobs),
			Paused: paused(c.Request().Context(), target),
		}

		revision, found, err := target.Store.CurrentRevision(c.Request().Context())
		if err == nil && found {
			row.SHA = revision.SHA
		}

		rows = append(rows, row)
	}

	//nolint:wrapcheck // echo writes the response; an encoding failure is reported verbatim
	return c.JSON(http.StatusOK, rows)
}

// handleAPIGet answers with what is being served, which is what a set diffs against and what compare-and-set then names.
func (s *Server) handleAPIGet(c echo.Context) error {
	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return echo.NewHTTPError(http.StatusNotFound, ErrNoSuchPipeline.Error())
	}

	revision, found, err := target.Store.CurrentRevision(c.Request().Context())
	if err != nil {
		return echoError(err)
	}

	if !found {
		return echo.NewHTTPError(http.StatusNotFound, "this pipeline has no configuration set")
	}

	//nolint:wrapcheck // as above
	return c.JSON(http.StatusOK, PipelineConfig{
		Name:     target.Slug,
		SHA:      revision.SHA,
		Source:   revision.Source,
		Includes: revision.Includes,
		From:     target.Path(),
		Paused:   paused(c.Request().Context(), target),
	})
}

// handleAPISet validates the upload HERE, on the machine that will run it, which is the whole reason a set is not a row-write.
func (s *Server) handleAPISet(c echo.Context) error {
	manager := s.held()
	if manager == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	name := c.Param("pipeline")

	// The name is concatenated into /p/<name> and stored as an identity, so what breaks either is refused once rather than escaped at each use.
	err := config.ValidPipelineName(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	var req SetRequest

	err = c.Bind(&req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "could not read the uploaded configuration: "+err.Error())
	}

	if req.Source == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "the uploaded configuration is empty")
	}

	result, err := manager.Set(c.Request().Context(), name, req)
	if err != nil {
		return setError(err)
	}

	//nolint:wrapcheck // as above
	return c.JSON(http.StatusOK, result)
}

// setError separates three answers a sender acts on differently: it moved under you (409), it is wrong (422), the daemon broke (500).
func setError(err error) error {
	switch {
	case errors.Is(err, ErrRevisionMoved):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, ErrRefused):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	default:
		return echoError(err)
	}
}

// ErrRefused is the sender's to fix — unparseable, or naming something this machine cannot supply — which is why it is not a 500.
var ErrRefused = errors.New("the pipeline was refused")

// handleAPIDestroy forgets a pipeline and everything recorded under it.
func (s *Server) handleAPIDestroy(c echo.Context) error {
	manager := s.held()
	if manager == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	err := manager.Destroy(c.Request().Context(), c.Param("pipeline"))
	if err != nil {
		return destroyError(err)
	}

	return c.NoContent(http.StatusNoContent) //nolint:wrapcheck // as above
}

// handleAPIRename moves a pipeline's identity, keeping its history.
func (s *Server) handleAPIRename(c echo.Context) error {
	manager := s.held()
	if manager == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	var body struct {
		To string `json:"to"`
	}

	err := c.Bind(&body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "could not read the new name: "+err.Error())
	}

	err = config.ValidPipelineName(body.To)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	err = manager.Rename(c.Request().Context(), c.Param("pipeline"), body.To)
	if err != nil {
		return destroyError(err)
	}

	return c.NoContent(http.StatusNoContent) //nolint:wrapcheck // as above
}

// destroyError maps a verb about a pipeline that must already exist.
func destroyError(err error) error {
	if errors.Is(err, ErrNoSuchPipeline) || errors.Is(err, store.ErrNoSuchPipeline) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	if errors.Is(err, ErrRefused) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}

	return echoError(err)
}

// handleAPIPause throws the pipeline-level breaker.
func (s *Server) handleAPIPause(c echo.Context) error { return s.setPaused(c, true) }

// handleAPIUnpause releases it.
func (s *Server) handleAPIUnpause(c echo.Context) error { return s.setPaused(c, false) }

// Through the store rather than the manager: it is one column on a row this package already holds a handle to.
func (s *Server) setPaused(c echo.Context, pause bool) error {
	if s.held() == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server holds no pipelines of its own")
	}

	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return echo.NewHTTPError(http.StatusNotFound, ErrNoSuchPipeline.Error())
	}

	var err error

	if pause {
		err = target.Store.Pause(c.Request().Context())
	} else {
		err = target.Store.Unpause(c.Request().Context())
	}

	if err != nil {
		return echoError(err)
	}

	return c.NoContent(http.StatusNoContent) //nolint:wrapcheck // as above
}

func echoError(err error) error {
	return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
}
