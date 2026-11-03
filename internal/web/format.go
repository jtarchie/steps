package web

import (
	"fmt"
	"strconv"
)

// FormatBinaryBytes renders a disk or transfer size in BINARY units,
// deliberately unlike formatBytes.
//
// That one is decimal so an agent's payload is comparable to the byte limits
// it is bounded by. This is about disks and wire transfers, which every tool
// a reader will cross-check against — the EC2 console, df, docker system df —
// reports in KiB/MiB/GiB.
func FormatBinaryBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	units := [...]string{"KiB", "MiB", "GiB", "TiB"}

	// The step up happens where the RENDERED value would, not where the exact
	// one does: %.1f rounds, so anything from 1023.95 up prints as "1024.0" —
	// a size back over the unit boundary it was just divided under. One byte
	// short of a GiB read as 1024.0 MiB.
	size, exp := float64(n)/unit, 0
	for size >= unit-0.05 && exp < len(units)-1 {
		size, exp = size/unit, exp+1
	}

	return fmt.Sprintf("%.1f %s", size, units[exp])
}
