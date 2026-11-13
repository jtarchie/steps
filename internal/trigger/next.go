package trigger

import (
	"time"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
)

// NextFire is when a cron resource next makes its jobs due, for a front end that may not import internal/resource (see resource.NextFire).
func NextFire(res config.Resource, now time.Time) (time.Time, error) {
	return rsrc.NextFire(res, now) //nolint:wrapcheck // resource.Cron names the resource
}
