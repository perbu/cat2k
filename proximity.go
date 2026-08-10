package main

import (
	"math"
	"time"
)

// Inter-cat proximity: together-episodes, patrol shifts, lead/follow
// (GitHub issue #6).
//
// The two collars do not sample at aligned timestamps, so both gated tracks
// (FixGPS only — snapped CMD-X zone fixes would fabricate togetherness at the
// zone center) are interpolated onto a shared 60s grid. Interpolation is
// honest: a grid point is only filled when its bracketing fixes are at most
// proxMaxInterpGapS apart; wider gaps leave the point missing, so neither the
// distance series nor a together-episode ever spans a signal outage.
//
// Together-episodes are runs of grid points where the cats are within
// proxTogetherDistM of each other, sustained for at least proxTogetherMinDurS.
//
// The lead/follow statistic is the departure-follow fraction from the issue's
// example copy, built on presence.go's home/away intervals: of the leader's
// excursions where the follower was verifiably home at departure time, the
// share where the follower left the home zone within followWindowS. This was
// chosen over cross-correlating movement vectors at lags 0-15 min because it
// needs no aligned movement data (the presence stream is denser than GPS) and
// answers the question owners actually ask. Excursions where the follower was
// already out — or its state is unknown — are ineligible: a cat that isn't
// home cannot follow, and an unknown state must not fabricate one.

const (
	// proxGridStepS: resolution of the shared time grid.
	proxGridStepS = 60.0
	// proxMaxInterpGapS: a grid point is interpolated only when its
	// bracketing fixes are at most this far apart; wider gaps stay missing.
	proxMaxInterpGapS = 180.0
	// proxTogetherDistM: grid points closer than this count as "together".
	// Well above the GPS jitter of two adjacent collars.
	proxTogetherDistM = 20.0
	// proxTogetherMinDurS: a close run must span at least this long to count
	// as a together-episode (crossing paths is not traveling together).
	proxTogetherMinDurS = 300.0
	// proxMaxWindowS: max span served by /api/proximity (payload bound).
	proxMaxWindowS = 48 * 3600.0
	// proxMaxEpisodes: cap on episodes per response (earliest kept).
	proxMaxEpisodes = 100
	// proxMaxEpisodePoints: per-cat polyline decimation cap per episode.
	proxMaxEpisodePoints = 120
	// followWindowS: a follower's departure within this long after the
	// leader's counts as following.
	followWindowS = 600.0
)

// proxFix is a gated GPS fix in projected meters (both cats share one frame).
type proxFix struct {
	t    time.Time
	x, y float64
}

// gateProximityFixes keeps FixGPS fixes and projects them with proj. Input
// must be time-ordered (the gate-eligible range queries are).
func gateProximityFixes(positions []DwellPosition, proj localProjection) []proxFix {
	var fixes []proxFix
	for _, p := range positions {
		if ClassifyFix(p.ValidSignal, p.Type) != FixGPS {
			continue
		}
		x, y := proj.toXY(p.Latitude, p.Longitude)
		fixes = append(fixes, proxFix{t: p.Timestamp, x: x, y: y})
	}
	return fixes
}

// gridSample is one cat's position on the shared grid; ok=false marks an
// honest gap (no fix pair close enough to interpolate across).
type gridSample struct {
	t    time.Time
	x, y float64
	ok   bool
}

// resampleTrack interpolates time-ordered fixes onto the shared grid
// [start, end] stepping proxGridStepS. A grid point landing exactly on a fix
// always takes that fix; otherwise it is linearly interpolated between its
// bracketing fixes, but only when those fixes are at most proxMaxInterpGapS
// apart — wider gaps leave the point missing.
func resampleTrack(fixes []proxFix, start, end time.Time) []gridSample {
	var out []gridSample
	step := time.Duration(proxGridStepS * float64(time.Second))

	i := 0 // fixes[i] is the last fix at or before the current grid time
	for t := start; !t.After(end); t = t.Add(step) {
		s := gridSample{t: t}
		for i+1 < len(fixes) && !fixes[i+1].t.After(t) {
			i++
		}
		if i < len(fixes) && !fixes[i].t.After(t) {
			switch {
			case fixes[i].t.Equal(t):
				s.x, s.y, s.ok = fixes[i].x, fixes[i].y, true
			case i+1 < len(fixes) && fixes[i+1].t.Sub(fixes[i].t).Seconds() <= proxMaxInterpGapS:
				f := t.Sub(fixes[i].t).Seconds() / fixes[i+1].t.Sub(fixes[i].t).Seconds()
				s.x = fixes[i].x + f*(fixes[i+1].x-fixes[i].x)
				s.y = fixes[i].y + f*(fixes[i+1].y-fixes[i].y)
				s.ok = true
			}
		}
		out = append(out, s)
	}
	return out
}

// DistancePoint is one grid point of the inter-cat distance series.
type DistancePoint struct {
	T         time.Time `json:"t"`
	DistanceM float64   `json:"distance_m"`
}

// TogetherEpisode is one sustained close run, with both cats' paired track
// segments ([lat, lon]) so the map can draw them without a second call.
type TogetherEpisode struct {
	Start     time.Time    `json:"start"`
	End       time.Time    `json:"end"`
	DurationS float64      `json:"duration_s"`
	PointsA   [][2]float64 `json:"points_a"`
	PointsB   [][2]float64 `json:"points_b"`
}

// proximitySeries pairs the two cats' grid samples: a distance point wherever
// both have data, and together-episodes for close runs spanning at least
// proxTogetherMinDurS. Missing grid points (honest gaps) break both the
// series and any run — silence is not proximity evidence. Episode point lists
// are stride-decimated to proxMaxEpisodePoints and the episode list is capped
// at proxMaxEpisodes to bound the payload.
func proximitySeries(gridA, gridB []gridSample, proj localProjection) ([]DistancePoint, []TogetherEpisode) {
	series := []DistancePoint{}
	episodes := []TogetherEpisode{}

	runStart := -1 // grid index where the current close run began
	flush := func(end int) {
		if runStart < 0 {
			return
		}
		if gridA[end].t.Sub(gridA[runStart].t).Seconds() >= proxTogetherMinDurS {
			ep := TogetherEpisode{Start: gridA[runStart].t, End: gridA[end].t}
			ep.DurationS = ep.End.Sub(ep.Start).Seconds()
			n := end - runStart + 1
			stride := 1
			if n > proxMaxEpisodePoints {
				stride = int(math.Ceil(float64(n) / float64(proxMaxEpisodePoints)))
			}
			for k := runStart; k <= end; k += stride {
				latA, lonA := proj.toLatLon(gridA[k].x, gridA[k].y)
				latB, lonB := proj.toLatLon(gridB[k].x, gridB[k].y)
				ep.PointsA = append(ep.PointsA, [2]float64{latA, lonA})
				ep.PointsB = append(ep.PointsB, [2]float64{latB, lonB})
			}
			episodes = append(episodes, ep)
		}
		runStart = -1
	}

	for k := range gridA {
		if !gridA[k].ok || !gridB[k].ok {
			flush(k - 1)
			continue
		}
		d := math.Hypot(gridA[k].x-gridB[k].x, gridA[k].y-gridB[k].y)
		series = append(series, DistancePoint{T: gridA[k].t, DistanceM: math.Round(d*10) / 10})
		if d < proxTogetherDistM {
			if runStart < 0 {
				runStart = k
			}
		} else {
			flush(k - 1)
		}
	}
	flush(len(gridA) - 1)

	if len(episodes) > proxMaxEpisodes {
		episodes = episodes[:proxMaxEpisodes]
	}
	return series, episodes
}

// Shift-cell states for the patrol-shift grid.
const (
	ShiftNeither = "neither" // both cats known, neither out (both home)
	ShiftA       = "a"       // only the first cat out
	ShiftB       = "b"       // only the second cat out
	ShiftBoth    = "both"    // both cats out
	ShiftNoData  = "nodata"  // either cat has no zone-known coverage that hour
)

// ShiftDay is one calendar day of the patrol-shift grid.
type ShiftDay struct {
	Date  string   `json:"date"`
	Hours []string `json:"hours"` // 24 cells, Shift* values
}

// computeShiftGrid buckets both cats' presence intervals into day x hour
// cells colored by who was out. A cat counts as out for an hour when its away
// coverage exceeds its home coverage inside that hour (majority of the
// hour's known zone time). An hour where either cat has no zone-known
// coverage at all is ShiftNoData rather than fabricated.
func computeShiftGrid(intA, intB []PresenceInterval, dates []string, loc *time.Location) []ShiftDay {
	days := make([]ShiftDay, len(dates))
	for i, date := range dates {
		days[i] = ShiftDay{Date: date, Hours: make([]string, 24)}
		dayStart, err := time.ParseInLocation("2006-01-02", date, loc)
		if err != nil {
			for h := range days[i].Hours {
				days[i].Hours[h] = ShiftNoData
			}
			continue
		}
		for h := 0; h < 24; h++ {
			hStart := dayStart.Add(time.Duration(h) * time.Hour)
			hEnd := hStart.Add(time.Hour)
			outA, knownA := hourOut(intA, hStart, hEnd)
			outB, knownB := hourOut(intB, hStart, hEnd)
			switch {
			case !knownA || !knownB:
				days[i].Hours[h] = ShiftNoData
			case outA && outB:
				days[i].Hours[h] = ShiftBoth
			case outA:
				days[i].Hours[h] = ShiftA
			case outB:
				days[i].Hours[h] = ShiftB
			default:
				days[i].Hours[h] = ShiftNeither
			}
		}
	}
	return days
}

// hourOut reports whether away coverage exceeds home coverage within
// [hStart, hEnd), and whether any zone-known coverage exists there at all.
func hourOut(intervals []PresenceInterval, hStart, hEnd time.Time) (out, known bool) {
	var homeS, awayS float64
	for _, iv := range intervals {
		s := iv.Start
		if s.Before(hStart) {
			s = hStart
		}
		e := iv.End
		if e.After(hEnd) {
			e = hEnd
		}
		if !e.After(s) {
			// A zero-length interval (single fix) inside the hour still
			// counts as known coverage.
			if iv.Start.Equal(iv.End) && !iv.Start.Before(hStart) && iv.Start.Before(hEnd) {
				known = true
			}
			continue
		}
		known = true
		if iv.State == PresenceAway {
			awayS += e.Sub(s).Seconds()
		} else {
			homeS += e.Sub(s).Seconds()
		}
	}
	return awayS > homeS, known
}

// FollowStat is one direction of the lead/follow relationship (see the file
// header for the statistic).
type FollowStat struct {
	LeaderID   int     `json:"leader_id"`
	Leader     string  `json:"leader"`
	FollowerID int     `json:"follower_id"`
	Follower   string  `json:"follower"`
	Departures int     `json:"departures"` // eligible leader excursions
	Followed   int     `json:"followed"`   // of those, followed within followWindowS
	Share      float64 `json:"share"`      // followed / departures (0 when no departures)
}

// followStats computes the departure-follow statistic in one direction over
// the cats' presence intervals (time-ordered, from derivePresenceIntervals):
// for each of the leader's away intervals (an excursion, its Start the
// departure), the follower must be verifiably home at that moment to be
// eligible, and follows when its first later departure falls within
// followWindowS.
func followStats(leader, follower []PresenceInterval) (departures, followed int) {
	for _, ex := range leader {
		if ex.State != PresenceAway {
			continue
		}
		state, known := presenceStateAt(follower, ex.Start)
		if !known || state != PresenceHome {
			continue
		}
		departures++
		for _, iv := range follower {
			if iv.State != PresenceAway || !iv.Start.After(ex.Start) {
				continue
			}
			if iv.Start.Sub(ex.Start).Seconds() <= followWindowS {
				followed++
			}
			break // only the follower's first later departure can follow
		}
	}
	return departures, followed
}

// presenceStateAt returns the state of the interval covering t.
func presenceStateAt(intervals []PresenceInterval, t time.Time) (PresenceState, bool) {
	for _, iv := range intervals {
		if !t.Before(iv.Start) && !t.After(iv.End) {
			return iv.State, true
		}
	}
	return PresenceUnknown, false
}
