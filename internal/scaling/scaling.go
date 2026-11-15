// Package scaling holds the pure DTN-count decisions used by the scaling
// Lambdas (#52), so they can be unit-tested without AWS.
package scaling

import cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

// DesiredDTNs is how many DTNs the endpoint needs right now: 0 with no active
// transfers; otherwise enough that aggregate throughput stays under the
// per-DTN threshold, at least 1 and at most max (#26).
//
// bps is the sum of effective_bytes_per_second over active tasks (a lifetime
// average per task, so it lags saturation); perDTNbps is the per-DTN scale-out
// threshold (config.Resolved.ThroughputThresholdBps).
func DesiredDTNs(active int, bps, perDTNbps int64, max int) int {
	if active <= 0 || max <= 0 {
		return 0
	}
	n := 1
	if perDTNbps > 0 && bps > perDTNbps {
		n = int((bps + perDTNbps - 1) / perDTNbps) // ceil
	}
	if n > max {
		n = max
	}
	return n
}

// ScaleUp returns the capacity to set when demand calls for target DTNs while
// current are desired: only ever upward (scale-in is the idle alarm's job).
func ScaleUp(current, target int) (int, bool) {
	if target > current {
		return target, true
	}
	return current, false
}

// AnyPositive reports whether any datapoint is > 0. CloudWatch returns
// datapoints unordered and a window can straddle two periods, so checking only
// the first one is wrong (#52).
func AnyPositive(values []float64) bool {
	for _, v := range values {
		if v > 0 {
			return true
		}
	}
	return false
}

// Maxima returns the Maximum statistic of each datapoint.
func Maxima(dps []cwtypes.Datapoint) []float64 {
	out := make([]float64, 0, len(dps))
	for _, d := range dps {
		if d.Maximum != nil {
			out = append(out, *d.Maximum)
		}
	}
	return out
}
