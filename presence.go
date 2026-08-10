package main

import (
	"time"
)

// WiFi-zone presence timeline (GitHub issue #4).
//
// The collar reports WiFi-zone presence directly: a fix with wifi_zone_id != 0
// is inside the home zone (on the property), a fix with wifi_zone_id == 0 (or
// wifi_zone_id_exited set) is outside it. Walking the fix stream and cutting a
// new interval at every state change yields the home/away timeline — free
// at-home detection, no GPS geofencing on noisy fixes needed.
//
// The WiFi zone is the single source of truth for presence state. The SureHub
// flap's inside/outside status is deliberately NOT blended in: it is
// known-stale (the owners often let the cats out themselves, so no flap event
// exists for many exits; and if SureHub wrongly believes a cat is outside it
// never records the exit, leaving its state stale indefinitely). Flap status
// stays on the status page as raw display-only data, exactly as before. A
// "garden" state (in zone + flap says out) was considered and dropped for this
// reason.
//
// Data caveats, handled deliberately:
//   - NULL wifi_zone_id means a pre-migration row (issue #1 backfill pending):
//     it carries no presence evidence. Such fixes are skipped entirely — they
//     neither extend nor break an interval — so historical ribbons simply show
//     nothing where zone data is unknown instead of fabricating state.
//   - Signal gaps: an interval may span at most presenceMaxGapS of silence. A
//     longer stretch without fixes ends the interval at the last fix before
//     the gap; the state during the gap itself is unknown, not assumed.

const (
	// presenceMaxGapS: a silence between consecutive zone-known fixes longer
	// than this ends the current interval. The collar reports every few
	// minutes when tracking; an hour of silence says nothing about state.
	presenceMaxGapS = 3600.0
	// presenceLookbackDays: how far back the /api/status chip walks the fix
	// stream to find the start of the ongoing state run.
	presenceLookbackDays = 14
)

// PresenceState is a cat's whereabouts relative to the home WiFi zone.
type PresenceState string

const (
	// PresenceHome: inside the home WiFi zone (on the property).
	PresenceHome PresenceState = "home"
	// PresenceAway: outside the home WiFi zone.
	PresenceAway PresenceState = "away"
	// PresenceUnknown: no usable zone data.
	PresenceUnknown PresenceState = "unknown"
)

// PresencePosition is the minimal per-fix data for presence analysis.
type PresencePosition struct {
	Timestamp        time.Time
	WifiZoneID       *int
	WifiZoneIDExited *int
}

// zoneState classifies one fix. known=false for pre-migration rows with NULL
// wifi_zone_id: those carry no presence evidence and must not fabricate
// intervals. An exited marker wins over the zone id itself (the API may still
// carry the old zone id on the departure fix).
func zoneState(p PresencePosition) (state PresenceState, known bool) {
	if p.WifiZoneID == nil {
		return PresenceUnknown, false
	}
	if p.WifiZoneIDExited != nil && *p.WifiZoneIDExited != 0 {
		return PresenceAway, true
	}
	if *p.WifiZoneID != 0 {
		return PresenceHome, true
	}
	return PresenceAway, true
}

// PresenceInterval is one maximal run of consecutive fixes in the same state.
// End is the timestamp of the run's last fix; a single-fix run has End == Start.
type PresenceInterval struct {
	Start time.Time     `json:"start"`
	End   time.Time     `json:"end"`
	State PresenceState `json:"state"`
}

// derivePresenceIntervals walks time-ordered fixes and cuts a new interval at
// every zone-state change. Fixes without zone data (NULL) are skipped; gaps
// longer than presenceMaxGapS between zone-known fixes close the current
// interval so it never silently spans a signal outage.
func derivePresenceIntervals(fixes []PresencePosition) []PresenceInterval {
	var intervals []PresenceInterval
	var cur *PresenceInterval
	var lastKnown time.Time

	flush := func() {
		if cur != nil {
			intervals = append(intervals, *cur)
			cur = nil
		}
	}

	for _, f := range fixes {
		state, known := zoneState(f)
		if !known {
			continue
		}
		if cur != nil && f.Timestamp.Sub(lastKnown).Seconds() > presenceMaxGapS {
			flush()
		}
		lastKnown = f.Timestamp

		if cur == nil {
			cur = &PresenceInterval{Start: f.Timestamp, End: f.Timestamp, State: state}
			continue
		}
		if state == cur.State {
			cur.End = f.Timestamp
		} else {
			flush()
			cur = &PresenceInterval{Start: f.Timestamp, End: f.Timestamp, State: state}
		}
	}
	flush()

	return intervals
}

// PresenceDay is one calendar day of the timeline plus its summary stats.
// Intervals are clipped to the day; excursion stats attribute each excursion
// to the day it departed and keep its full (unclipped) duration.
type PresenceDay struct {
	Date              string             `json:"date"`
	Intervals         []PresenceInterval `json:"intervals"`
	TimeHomeS         float64            `json:"time_home_s"`
	Excursions        int                `json:"excursions"`
	LongestExcursionS float64            `json:"longest_excursion_s"`
}

// summarizePresenceDays buckets intervals into the given calendar dates
// (YYYY-MM-DD in loc, any order) and computes the per-day summary. Days with
// no zone-known fixes get an empty interval list and zeroed stats.
func summarizePresenceDays(intervals []PresenceInterval, dates []string, loc *time.Location) []PresenceDay {
	days := make([]PresenceDay, len(dates))
	for i, date := range dates {
		days[i] = PresenceDay{Date: date, Intervals: []PresenceInterval{}}
		dayStart, err := time.ParseInLocation("2006-01-02", date, loc)
		if err != nil {
			continue
		}
		dayEnd := dayStart.AddDate(0, 0, 1)

		for _, iv := range intervals {
			if iv.Start.Before(dayEnd) && iv.End.After(dayStart) ||
				iv.Start.Equal(iv.End) && !iv.Start.Before(dayStart) && iv.Start.Before(dayEnd) {
				s := iv.Start
				if s.Before(dayStart) {
					s = dayStart
				}
				e := iv.End
				if e.After(dayEnd) {
					e = dayEnd
				}
				days[i].Intervals = append(days[i].Intervals, PresenceInterval{Start: s, End: e, State: iv.State})
				if iv.State == PresenceHome {
					days[i].TimeHomeS += e.Sub(s).Seconds()
				}
			}

			// Excursions count on the day they departed.
			if iv.State == PresenceAway && !iv.Start.Before(dayStart) && iv.Start.Before(dayEnd) {
				days[i].Excursions++
				if d := iv.End.Sub(iv.Start).Seconds(); d > days[i].LongestExcursionS {
					days[i].LongestExcursionS = d
				}
			}
		}
	}
	return days
}

// Excursion is one away interval: a trip off the property.
type Excursion struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	DurationS float64   `json:"duration_s"`
}

// excursionDurationBucketS are the exclusive upper edges of the duration
// histogram buckets, in seconds: <5m, 5-15m, 15-30m, 30-60m, 1-2h, 2-4h,
// 4-8h, and a final catch-all >=8h. Sent to the client so it can label bars.
var excursionDurationBucketS = []float64{300, 900, 1800, 3600, 7200, 14400, 28800}

// ExcursionStats aggregates the away intervals of a period for the stats
// panel: the raw excursion list, a duration histogram, and a departure
// hour-of-day histogram (the cat's daily rhythm).
type ExcursionStats struct {
	Excursions      []Excursion `json:"excursions"`
	DurationBucketS []float64   `json:"duration_bucket_s"` // upper edge per bucket (last bucket: catch-all)
	DurationCounts  []int       `json:"duration_counts"`   // len(DurationBucketS)+1
	DepartureHours  [24]int     `json:"departure_hours"`   // excursions per local hour of departure
}

// computeExcursions extracts excursions (away intervals) from a timeline and
// aggregates the histograms. Zero-duration excursions (a single away fix)
// count too: the cat really was seen off the property, its trip just wasn't
// bracketed by two fixes.
func computeExcursions(intervals []PresenceInterval, loc *time.Location) ExcursionStats {
	stats := ExcursionStats{
		Excursions:      []Excursion{},
		DurationBucketS: excursionDurationBucketS,
		DurationCounts:  make([]int, len(excursionDurationBucketS)+1),
	}
	for _, iv := range intervals {
		if iv.State != PresenceAway {
			continue
		}
		dur := iv.End.Sub(iv.Start).Seconds()
		stats.Excursions = append(stats.Excursions, Excursion{Start: iv.Start, End: iv.End, DurationS: dur})

		bucket := len(excursionDurationBucketS)
		for b, edge := range excursionDurationBucketS {
			if dur < edge {
				bucket = b
				break
			}
		}
		stats.DurationCounts[bucket]++
		stats.DepartureHours[iv.Start.In(loc).Hour()]++
	}
	return stats
}

// currentPresence finds the ongoing state and when it began by walking the
// fix stream backwards from the most recent zone-known fix. known=false when
// no fix carries zone data (e.g. only pre-migration rows). The walk stops at
// the first state change or gap over presenceMaxGapS, so "since" is the start
// of the current continuous run, not just the latest fix.
func currentPresence(fixes []PresencePosition) (PresenceState, time.Time, bool) {
	i := len(fixes) - 1
	for ; i >= 0; i-- {
		if _, known := zoneState(fixes[i]); known {
			break
		}
	}
	if i < 0 {
		return PresenceUnknown, time.Time{}, false
	}

	state, _ := zoneState(fixes[i])
	since := fixes[i].Timestamp
	prev := fixes[i].Timestamp
	for j := i - 1; j >= 0; j-- {
		s, known := zoneState(fixes[j])
		if !known {
			continue
		}
		if s != state || prev.Sub(fixes[j].Timestamp).Seconds() > presenceMaxGapS {
			break
		}
		since = fixes[j].Timestamp
		prev = fixes[j].Timestamp
	}
	return state, since, true
}
