// Package telemetry is the Telemetry bounded context: turning platform
// counters into rates, and choosing which stored resolution answers a query.
//
// Both rules are pure functions. They decide what the charts show, and getting
// them wrong produces plausible-looking nonsense rather than an obvious error,
// so they are kept here where they can be tested exhaustively.
package telemetry

import "time"

// Resolution names a stored sampling granularity.
type Resolution string

const (
	ResolutionRaw Resolution = "raw" // 1 minute, kept 48h
	Resolution5m  Resolution = "5m"  // kept 30 days
	Resolution30m Resolution = "30m" // kept 180 days
	Resolution3h  Resolution = "3h"  // kept 400 days
)

// Retention per resolution, mirroring the Timescale policies in migration
// 00005. Queries use these to avoid asking for data that has been dropped.
var retention = map[Resolution]time.Duration{
	ResolutionRaw: 48 * time.Hour,
	Resolution5m:  30 * 24 * time.Hour,
	Resolution30m: 180 * 24 * time.Hour,
	Resolution3h:  400 * 24 * time.Hour,
}

// Bucket is the time width of one point at a resolution.
var bucket = map[Resolution]time.Duration{
	ResolutionRaw: time.Minute,
	Resolution5m:  5 * time.Minute,
	Resolution30m: 30 * time.Minute,
	Resolution3h:  3 * time.Hour,
}

// TargetPoints is roughly how many points a chart wants: enough to show shape,
// few enough to stay responsive and legible. 400 is chosen so the common
// windows land on the resolution an operator would pick by hand - a week gets
// 30-minute buckets (336 points) rather than being flattened to three-hourly.
const TargetPoints = 400

// MaxPoints caps a response so a pathological range cannot return millions of
// rows to a browser. A full year at the coarsest stored resolution is ~2,920
// points, so the cap sits above that.
const MaxPoints = 3000

// SelectResolution picks the coarsest resolution that still gives a useful
// number of points for the requested window, and never picks one whose
// retention has already dropped the data.
//
// Choosing by window rather than by a fixed mapping means a 12-hour chart stays
// detailed while a year-long chart stays fast, without the caller knowing which
// aggregate exists.
func SelectResolution(from, to time.Time, now time.Time) Resolution {
	window := to.Sub(from)
	if window <= 0 {
		return ResolutionRaw
	}
	age := now.Sub(from)

	// Coarsest first, so the cheapest table that satisfies the request wins.
	for _, r := range []Resolution{Resolution3h, Resolution30m, Resolution5m, ResolutionRaw} {
		if age > retention[r] {
			continue // this resolution no longer holds data that old
		}
		if points := int(window / bucket[r]); points <= MaxPoints {
			// Prefer the finest resolution that fits the point budget: walk
			// down from here to the smallest bucket that still fits.
			return finestFitting(window, age)
		}
	}
	return Resolution3h
}

func finestFitting(window, age time.Duration) Resolution {
	for _, r := range []Resolution{ResolutionRaw, Resolution5m, Resolution30m, Resolution3h} {
		if age > retention[r] {
			continue
		}
		if int(window/bucket[r]) <= TargetPoints {
			return r
		}
	}
	return Resolution3h
}

// Bucket returns the time width of one point at a resolution.
func Bucket(r Resolution) time.Duration { return bucket[r] }

// Retention returns how long a resolution is kept.
func Retention(r Resolution) time.Duration { return retention[r] }

// CounterReading is one cumulative counter sample from a platform.
type CounterReading struct {
	Previous     float64
	PreviousTime time.Time
	Current      float64
	CurrentTime  time.Time
}

// Rate converts two cumulative counter readings into a per-second rate.
//
// It reports ok=false rather than guessing when the reading cannot yield a
// meaningful rate:
//
//   - no previous reading: the first sample after a restart has nothing to
//     compare against
//   - the counter went backwards: the guest rebooted or the counter wrapped, so
//     the difference is meaningless and would render as an enormous spike
//   - zero or negative elapsed time: clock skew between nodes
//
// Dropping a sample leaves a one-point gap in a chart. Inventing one puts a
// fictional spike in a capacity report.
func Rate(r CounterReading) (perSecond float64, ok bool) {
	if r.PreviousTime.IsZero() {
		return 0, false
	}
	elapsed := r.CurrentTime.Sub(r.PreviousTime).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	if r.Current < r.Previous {
		return 0, false
	}
	return (r.Current - r.Previous) / elapsed, true
}

// MaxCounterGap is how stale a previous reading may be and still produce a
// rate. Beyond this the average is over so long a window that it hides
// everything that happened inside it.
const MaxCounterGap = 15 * time.Minute

// RateWithinGap is Rate, additionally rejecting readings too far apart to
// average meaningfully.
func RateWithinGap(r CounterReading) (float64, bool) {
	if !r.PreviousTime.IsZero() && r.CurrentTime.Sub(r.PreviousTime) > MaxCounterGap {
		return 0, false
	}
	return Rate(r)
}
