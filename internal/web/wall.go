package web

import (
	"math"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

const wallPath = "/wall"

// wallGrid is the columns and rows that put n tiles on one 16:9 screen: columns grow with the square root of the count, so the tiles stay roughly the screen's shape however many there are.
func wallGrid(n int) (cols, rows int) {
	if n < 1 {
		return 1, 1
	}

	cols = min(n, int(math.Ceil(math.Sqrt(float64(n)*16/9))))

	return cols, (n + cols - 1) / cols
}

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

// Class is the tile's one state, red outranking paused outranking building, as the root's mark ranks them.
func (w wallTile) Class() string {
	switch {
	case w.Red():
		return "red"
	case w.Paused:
		return "paused"
	case w.Building():
		return "building"
	case !w.HasRun:
		return "never"
	default:
		return ""
	}
}

// Glyph draws the state in shape, so it reads without its colour.
func (w wallTile) Glyph() string {
	switch {
	case w.Red():
		return "✗"
	case w.Paused:
		return "‖"
	case w.Building():
		return "◐"
	case !w.HasRun:
		return "○"
	default:
		return "✓"
	}
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

// wallNameLen is the longest name the wall sizes its type for, so every name is one size and a tile is not read as smaller news; past wallNameCap a name is cut rather than shrinking every tile for one.
func wallNameLen(tiles []wallTile) int {
	longest := 1

	for _, tile := range tiles {
		longest = max(longest, len([]rune(tile.Slug)))
	}

	return min(longest, wallNameCap)
}

const wallNameCap = 24

// wallTally is the wall's head: what is red, building, paused, and the rest.
type wallTally struct {
	Total, Failing, Building, Paused, Green int
}

func tallyWall(tiles []wallTile) wallTally {
	tally := wallTally{Total: len(tiles)}

	for _, tile := range tiles {
		switch tile.Class() {
		case "red":
			tally.Failing++
		case "paused":
			tally.Paused++
		case "building":
			tally.Building++
		case "":
			tally.Green++
		}
	}

	return tally
}

// handleWall serves the radiator: every pipeline the reader may see as a tile, sized to one screen, with nothing on it to click but the tiles. It is the root's rows, so a tile and a row cannot disagree.
func (s *Server) handleWall(c *echo.Context) error {
	nav := s.globalNav(c)
	rows := s.overviewPipelines(c, nav)
	tiles := make([]wallTile, 0, len(rows))

	for _, row := range rows {
		tiles = append(tiles, wallTile{row})
	}

	cols, gridRows := wallGrid(len(tiles))

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "wall", map[string]any{
		"Nav":   nav,
		"Title": "wall",
		"Wall":  true,
		"Tiles": tiles,
		"Tally": tallyWall(tiles),
		"Cols":  cols,
		"Rows":  gridRows,
		"Len":   wallNameLen(tiles),
		"Now":   time.Now(),
	})
}
