// Package forecast turns a claim-arrival history into a warm pool target.
//
// Everything here is a pure function of its inputs so the reconciler stays stateless and a
// restarted controller computes the same target from the same history.
package forecast

import (
	"math"
)

// Params configures the smoothing model.
type Params struct {
	// Alpha is the level smoothing factor in (0, 1].
	Alpha float64
	// Beta is the trend smoothing factor in [0, 1]. Zero gives EWMA (no trend).
	Beta float64
}

// Smooth fits Holt's linear smoothing to series (arrivals per bucket, oldest first) and returns
// the final level and trend. With Beta == 0 it is EWMA and trend stays 0. An empty series
// yields (0, 0).
func Smooth(series []float64, p Params) (level, trend float64) {
	if len(series) == 0 {
		return 0, 0
	}
	level = series[0]
	for _, x := range series[1:] {
		prevLevel := level
		level = p.Alpha*x + (1-p.Alpha)*(prevLevel+trend)
		trend = p.Beta*(level-prevLevel) + (1-p.Beta)*trend
	}
	return level, trend
}

// ExpectedArrivals sums the forecast for the next horizon buckets. Per-bucket forecasts are
// floored at zero: a falling trend must not cancel demand earlier in the horizon.
func ExpectedArrivals(level, trend float64, horizon int) float64 {
	var sum float64
	for h := 1; h <= horizon; h++ {
		sum += math.Max(0, level+float64(h)*trend)
	}
	return sum
}

// Replicas converts expected arrivals into a pool size with Poisson-style headroom:
// ceil(expected + z*sqrt(expected)). Arrival counts are roughly Poisson, whose standard
// deviation is sqrt(mean), so z is the number of standard deviations covered.
func Replicas(expected, z float64) int32 {
	if expected <= 0 || math.IsNaN(expected) {
		return 0
	}
	v := math.Ceil(expected + z*math.Sqrt(expected))
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

// Clamp bounds v to [lo, hi]. If lo > hi, hi wins: maxReplicas is the hard budget ceiling.
func Clamp(v, lo, hi int64) int32 {
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	if v < 0 {
		v = 0
	}
	if v > math.MaxInt32 {
		v = math.MaxInt32
	}
	return int32(v)
}

// HorizonBuckets is the number of whole buckets covering leadSeconds (at least one).
func HorizonBuckets(leadSeconds, bucketSeconds int32) int {
	if bucketSeconds <= 0 {
		return 1
	}
	n := int((leadSeconds + bucketSeconds - 1) / bucketSeconds)
	if n < 1 {
		return 1
	}
	return n
}
