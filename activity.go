package main

import (
	"sort"
	"time"
)

// Activity estimation tuning. These are deliberately simple, fixed thresholds:
// the goal is a *consistent* relative signal for spotting changes in a cat's
// behaviour (is it moving less than usual?), not metrologically accurate
// odometry. Consistency matters more than absolute correctness.
const (
	// activityMaxSpeedMS drops intervals implying an impossible speed (~72 km/h):
	// these are bad fixes, not travel.
	activityMaxSpeedMS = 20.0
	// activityMaxGapS ignores intervals spanning a long signal gap, so a single
	// jump across hours of missing data doesn't count as a straight-line sprint.
	activityMaxGapS = 1800.0
	// activityMinSats gates fixes with too few satellites (low-quality fixes).
	activityMinSats = 3
)

// ActivityPosition is a position with the fields needed to gate and measure it.
type ActivityPosition struct {
	TrackerID   int
	Timestamp   time.Time
	Latitude    float64
	Longitude   float64
	Satellites  *int
	ValidSignal *bool
}

// DayActivity holds the computed activity metrics for one calendar day.
type DayActivity struct {
	Date           string  `json:"date"`            // YYYY-MM-DD (local)
	DistanceM      float64 `json:"distance_m"`      // summed travel above the jitter floor
	ActiveFraction float64 `json:"active_fraction"` // fraction of intervals where the cat moved
	RangeM         float64 `json:"range_m"`         // furthest distance from home reached
	Fixes          int     `json:"fixes"`           // number of good fixes used
	Intervals      int     `json:"intervals"`       // measured intervals (denominator of active_fraction)
	ActiveIntervals int    `json:"active_intervals"` // intervals where the hop cleared the active floor
}

// activityGate reports whether a fix is good enough to use for measurement.
func activityGate(p ActivityPosition) bool {
	if p.ValidSignal != nil && !*p.ValidSignal {
		return false
	}
	if p.Satellites != nil && *p.Satellites < activityMinSats {
		return false
	}
	return true
}

// computeActivity bins gated positions by local calendar day and returns one
// DayActivity per requested date (oldest first), filling zero days where there
// is no data. dates must be YYYY-MM-DD strings in loc.
// Distance always counts every hop (no floor) so the bars reflect raw travel.
// activeFloorM only governs the active-fraction metric: an interval counts as
// "active" when its hop clears this floor, separating real movement from jitter.
func computeActivity(positions []ActivityPosition, dates []string, homeLat, homeLon, activeFloorM float64, loc *time.Location) []DayActivity {
	byDay := make(map[string][]ActivityPosition)
	for _, p := range positions {
		if !activityGate(p) {
			continue
		}
		d := p.Timestamp.In(loc).Format("2006-01-02")
		byDay[d] = append(byDay[d], p)
	}

	out := make([]DayActivity, len(dates))
	for i, d := range dates {
		ps := byDay[d]
		sort.Slice(ps, func(a, b int) bool { return ps[a].Timestamp.Before(ps[b].Timestamp) })
		out[i] = computeDayActivity(d, ps, homeLat, homeLon, activeFloorM)
	}
	return out
}

// computeDayActivity measures one day's worth of time-ordered, gated fixes.
func computeDayActivity(date string, ps []ActivityPosition, homeLat, homeLon, activeFloorM float64) DayActivity {
	day := DayActivity{Date: date, Fixes: len(ps)}
	homeSet := homeLat != 0 || homeLon != 0

	var prev *ActivityPosition
	total, active := 0, 0

	for i := range ps {
		cur := ps[i]

		if homeSet {
			if r := haversineDistance(homeLat, homeLon, cur.Latitude, cur.Longitude); r > day.RangeM {
				day.RangeM = r
			}
		}

		if prev != nil {
			dt := cur.Timestamp.Sub(prev.Timestamp).Seconds()
			switch {
			case dt <= 0:
				// duplicate/disordered timestamp; advance reference, ignore
			case dt > activityMaxGapS:
				// long signal gap: start a fresh segment, don't measure across it
			default:
				d := haversineDistance(prev.Latitude, prev.Longitude, cur.Latitude, cur.Longitude)
				if d/dt > activityMaxSpeedMS {
					// implausible jump: treat cur as a bad fix, keep the last
					// good fix as the reference and skip this interval entirely.
					continue
				}
				total++
				day.DistanceM += d // distance counts every hop, no floor
				if d >= activeFloorM {
					active++
				}
			}
		}

		p := cur
		prev = &p
	}

	day.Intervals = total
	day.ActiveIntervals = active
	if total > 0 {
		day.ActiveFraction = float64(active) / float64(total)
	}
	return day
}
