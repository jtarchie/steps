package web

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v5"
)

const wallPath = "/wall"

// wallNameCap is the longest failing name the wall sizes its type for; past it a name is cut rather than shrinking every other sign for one.
const wallNameCap = 24

// wallTile is one pipeline on the wall: the root's row, read for a screen across the room.
type wallTile struct {
	overviewPipeline
}

// Building is a pipeline with a job in flight: on a green wall, the one thing about to change.
func (w wallTile) Building() bool {
	for _, chip := range w.Chips {
		if chip.Running {
			return true
		}
	}

	return false
}

// RunningJob is the first job in flight, in config order.
func (w wallTile) RunningJob() string {
	for _, chip := range w.Chips {
		if chip.Running {
			return chip.Name
		}
	}

	return ""
}

// wallSigns is the wall's answer, ordered by what wants a person: each failing pipeline its own sign, newest failure first, then one sign each for building, passing, paused and never run.
type wallSigns struct {
	Failing, Building, Passing, Paused, Never []wallTile
	Total                                     int
	// Len is the longest failing name, which every failing name is sized for, so one is not read as smaller news than another.
	Len int
}

// AllPassing is the quiet wall: every pipeline green, which is said once, large.
func (w wallSigns) AllPassing() bool { return w.Total > 0 && len(w.Passing) == w.Total }

// sortWall files each pipeline under the one sign its state belongs to, red outranking paused outranking building, as the root's mark ranks them.
func sortWall(rows []overviewPipeline) wallSigns {
	signs := wallSigns{Total: len(rows), Len: 1}

	for _, row := range rows {
		tile := wallTile{row}

		switch {
		case tile.Red():
			signs.Failing = append(signs.Failing, tile)
			signs.Len = max(signs.Len, min(len([]rune(tile.Slug)), wallNameCap))
		case tile.Paused:
			signs.Paused = append(signs.Paused, tile)
		case tile.Building():
			signs.Building = append(signs.Building, tile)
		case !tile.HasRun:
			signs.Never = append(signs.Never, tile)
		default:
			signs.Passing = append(signs.Passing, tile)
		}
	}

	slices.SortStableFunc(signs.Failing, func(a, b wallTile) int {
		return cmp.Compare(b.Latest.StartedAt.UnixNano(), a.Latest.StartedAt.UnixNano())
	})

	return signs
}

// handleWall serves the radiator: a highway message sign for the pipelines the reader may see, with nothing on it to press but the pipelines. It is the root's rows, so a sign and a row cannot disagree.
func (s *Server) handleWall(c *echo.Context) error {
	nav := s.globalNav(c)

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "wall", map[string]any{
		"Nav":   nav,
		"Title": "wall",
		"Wall":  true,
		"Signs": sortWall(s.overviewPipelines(c, nav)),
		"Now":   time.Now(),
	})
}
