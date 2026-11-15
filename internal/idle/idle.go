// Package idle decides whether an idle control node may be stopped (#52).
package idle

import (
	"fmt"
	"time"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/scaling"
)

// Inputs are the facts idle_shutdown gathers.
type Inputs struct {
	Running   bool
	Uptime    time.Duration
	MinUptime time.Duration
	// Desired and InService describe the DTN pool.
	Desired   int
	InService int
	// WarmStates are the warm pool instances' lifecycle states.
	WarmStates []string
	// ActiveTransfers holds the ActiveTransfers datapoints for the idle window;
	// empty means no data (the demand monitor isn't reporting).
	ActiveTransfers []float64
}

// ShouldStop reports whether to stop the control node, and why (or why not).
func ShouldStop(in Inputs) (bool, string) {
	switch {
	case !in.Running:
		return false, "control node not running"
	case in.Uptime < in.MinUptime:
		return false, fmt.Sprintf("within minimum uptime (%s < %s)", in.Uptime.Round(time.Minute), in.MinUptime)
	case in.Desired > 0:
		return false, fmt.Sprintf("DTN pool desired=%d", in.Desired)
	case in.InService > 0:
		return false, fmt.Sprintf("%d DTN(s) InService", in.InService)
	}
	for _, s := range in.WarmStates {
		if s != "Warmed:Stopped" && s != "Warmed:Terminated" {
			return false, "warm pool instance in " + s
		}
	}
	// Missing data means the demand monitor isn't reporting, not that the
	// endpoint is idle (#30).
	if len(in.ActiveTransfers) == 0 {
		return false, "no ActiveTransfers data in the idle window"
	}
	if scaling.AnyPositive(in.ActiveTransfers) {
		return false, "active transfers in the idle window"
	}
	return true, "idle"
}
