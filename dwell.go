package main

import (
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Dwell-point waypoints and territory transitions (GitHub issue #2).
//
// Pipeline: gate fixes (only genuine GPS fixes — snapped WiFi-zone fixes
// would fabricate a giant phantom waypoint at the zone center), group
// consecutive stationary fixes into dwell episodes, cluster episode centroids
// with a small pure-Go DBSCAN, then count transitions between consecutive
// episodes as a directed graph over the clusters.
//
// Name persistence: clustering runs on demand per API request, so cluster
// identity is recomputed each time. To keep user-assigned names stable across
// recomputation, detected clusters are matched against the `waypoints` table
// by proximity (nearest stored waypoint within dwellMatchRadiusM of the
// cluster centroid, claimed at most once per run). Matched rows keep their id
// and name and get their centroid refreshed; unmatched clusters insert a new
// row. Unnamed waypoints get a rank-based auto-label ("Waypoint 1" = most
// dwelt).
//
// Pruning: a place the cat has abandoned should not accumulate forever.
// Clusters whose last visit is older than waypointPruneDays are dropped from
// the response (and their stored row, if any, is deleted — the name goes with
// it), and stored rows matched by no cluster are deleted when no episode in
// the last waypointPruneDays lies near them. A genuinely revisited place is
// simply re-detected as a fresh waypoint.

const (
	// dwellStationaryRadiusM: a fix within this distance of the episode's
	// anchor fix counts as stationary (covers GPS jitter around a nap spot).
	dwellStationaryRadiusM = 30.0
	// dwellMinDurationS: a stationary run must span at least this long to
	// count as a dwell episode ("several minutes").
	dwellMinDurationS = 180.0
	// dwellMaxGapS: a gap between consecutive fixes longer than this ends
	// the episode (signal loss is not evidence of dwelling).
	dwellMaxGapS = 1200.0
	// dwellClusterEpsilonM: DBSCAN epsilon over episode centroids.
	dwellClusterEpsilonM = 20.0
	// dwellMatchRadiusM: max distance between a freshly computed cluster
	// centroid and a stored waypoint for them to count as the same place.
	// Roughly 2x epsilon: recomputed centroids drift as new data arrives.
	dwellMatchRadiusM = 40.0
	// waypointPruneDays: a waypoint with no dwell episode near it in this
	// many days is pruned — dropped from the response and deleted from the
	// waypoints table, name included.
	waypointPruneDays = 10
)

// DwellPosition is a position with the fields needed to gate and cluster it.
type DwellPosition struct {
	TrackerID   int
	Timestamp   time.Time
	Latitude    float64
	Longitude   float64
	ValidSignal *bool
	Type        *string
	WifiZoneID  *int
	Radius      *int
}

// dwellGate reports whether a fix is usable for dwell analysis. Only genuine
// GPS fixes pass: invalid fixes and snapped WiFi-zone fixes (FixHome) are
// excluded per gate.go.
func dwellGate(p DwellPosition) bool {
	return ClassifyFix(p.ValidSignal, p.Type, p.WifiZoneID) == FixGPS
}

// dwellEpisode is one continuous stationary stay: the cat stayed within
// dwellStationaryRadiusM of the anchor fix from start to end.
type dwellEpisode struct {
	start time.Time
	end   time.Time
	lat   float64 // centroid of the episode's fixes
	lon   float64
}

// detectDwellEpisodes walks time-ordered, gated fixes and groups consecutive
// stationary fixes into episodes. Grouping is anchor-based: each fix is
// compared to the first fix of the run, so a run cannot drift. Runs shorter
// than dwellMinDurationS are dropped (passing by is not dwelling). The walk
// advances past consumed fixes, so episodes never overlap.
func detectDwellEpisodes(fixes []DwellPosition) []dwellEpisode {
	var episodes []dwellEpisode

	for i := 0; i < len(fixes); {
		anchor := fixes[i]
		j := i + 1
		sumLat, sumLon := anchor.Latitude, anchor.Longitude

		for j < len(fixes) {
			if fixes[j].Timestamp.Sub(fixes[j-1].Timestamp).Seconds() > dwellMaxGapS {
				break
			}
			if haversineDistance(anchor.Latitude, anchor.Longitude, fixes[j].Latitude, fixes[j].Longitude) > dwellStationaryRadiusM {
				break
			}
			sumLat += fixes[j].Latitude
			sumLon += fixes[j].Longitude
			j++
		}

		n := float64(j - i)
		if fixes[j-1].Timestamp.Sub(anchor.Timestamp).Seconds() >= dwellMinDurationS {
			episodes = append(episodes, dwellEpisode{
				start: anchor.Timestamp,
				end:   fixes[j-1].Timestamp,
				lat:   sumLat / n,
				lon:   sumLon / n,
			})
		}

		i = j
	}

	return episodes
}

// dbscanEpisodes clusters episode centroids with DBSCAN and returns a cluster
// label per episode (0-based, in order of discovery). minPoints is 1: every
// dwell episode is a real stay, so even a singleton deserves a waypoint —
// DBSCAN then degenerates to epsilon-connected components, which is exactly
// the chaining behaviour wanted ("the hedge" and "the oak" 15 m apart are
// one place). O(n²) over episodes; trivial at this data volume.
func dbscanEpisodes(episodes []dwellEpisode, epsilonM float64) []int {
	labels := make([]int, len(episodes))
	for i := range labels {
		labels[i] = -1
	}

	cluster := 0
	for i := range episodes {
		if labels[i] != -1 {
			continue
		}
		labels[i] = cluster
		queue := []int{i}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for j := range episodes {
				if labels[j] != -1 {
					continue
				}
				if haversineDistance(episodes[cur].lat, episodes[cur].lon, episodes[j].lat, episodes[j].lon) <= epsilonM {
					labels[j] = cluster
					queue = append(queue, j)
				}
			}
		}
		cluster++
	}

	return labels
}

// Waypoint is a detected dwell point for the /api/waypoints response.
type Waypoint struct {
	ID           int64       `json:"id"`
	Name         string      `json:"name"`  // stored name, or auto-label when not named
	Named        bool        `json:"named"` // true when the name is user-assigned
	Latitude     float64     `json:"lat"`
	Longitude    float64     `json:"lon"`
	RadiusM      float64     `json:"radius_m"` // furthest episode centroid from the waypoint centroid
	DwellSeconds float64     `json:"dwell_seconds"`
	Share        float64     `json:"share"`      // share of total dwell time in the period
	PrevShare    float64     `json:"prev_share"` // share in the preceding period of the same length
	Visits       int         `json:"visits"`     // number of dwell episodes
	Hours        [24]float64 `json:"hours"`      // dwell seconds per local hour of day (episode start hour)
	LastVisit    time.Time   `json:"last_visit"`
}

// WaypointTransition is a directed edge in the territory graph.
type WaypointTransition struct {
	From        int64   `json:"from"` // waypoint ID
	To          int64   `json:"to"`   // waypoint ID
	Count       int     `json:"count"`
	Probability float64 `json:"probability"` // count / total outgoing transitions from From
}

// computeWaypoints runs the full dwell pipeline for one tracker over the last
// `days` days: detect episodes, cluster them, reconcile cluster identity with
// the waypoints table (preserving user-assigned names), and count transitions
// between consecutive episodes. Dwell share for the preceding window of the
// same length is included per waypoint so the UI can show a trend. loc is the
// timezone used for the time-of-day histogram.
func (d *Database) computeWaypoints(trackerID, days int, loc *time.Location) ([]Waypoint, []WaypointTransition, error) {
	now := time.Now()
	since := now.AddDate(0, 0, -days)
	prevSince := now.AddDate(0, 0, -2*days)
	pruneCutoff := now.AddDate(0, 0, -waypointPruneDays)

	// The fetch must reach back at least to the prune horizon so the
	// last-visit checks below have the episodes they need.
	fetchSince := prevSince
	if pruneCutoff.Before(fetchSince) {
		fetchSince = pruneCutoff
	}

	fixes, err := d.GetPositionsForDwell(trackerID, fetchSince)
	if err != nil {
		return nil, nil, err
	}

	// Gate and split into current and previous window. gated keeps the
	// whole fetched range for prune last-visit checks; fixes older than
	// the trend window are excluded from prev so prev_share keeps its
	// semantics.
	var gated, cur, prev []DwellPosition
	for _, p := range fixes {
		if !dwellGate(p) {
			continue
		}
		gated = append(gated, p)
		if p.Timestamp.Before(prevSince) {
			continue
		}
		if p.Timestamp.Before(since) {
			prev = append(prev, p)
		} else {
			cur = append(cur, p)
		}
	}

	episodes := detectDwellEpisodes(cur)
	labels := dbscanEpisodes(episodes, dwellClusterEpsilonM)

	// Aggregate episodes into clusters.
	type clusterAgg struct {
		sumLat, sumLon float64
		dwell          float64
		visits         int
		radius         float64
		hours          [24]float64
		lastVisit      time.Time
		lat, lon       float64
	}
	clusters := map[int]*clusterAgg{}
	for i, ep := range episodes {
		c := clusters[labels[i]]
		if c == nil {
			c = &clusterAgg{}
			clusters[labels[i]] = c
		}
		dur := ep.end.Sub(ep.start).Seconds()
		c.sumLat += ep.lat
		c.sumLon += ep.lon
		c.dwell += dur
		c.visits++
		c.hours[ep.start.In(loc).Hour()] += dur
		if ep.end.After(c.lastVisit) {
			c.lastVisit = ep.end
		}
	}
	for _, c := range clusters {
		c.lat = c.sumLat / float64(c.visits)
		c.lon = c.sumLon / float64(c.visits)
	}
	for i, ep := range episodes {
		c := clusters[labels[i]]
		if r := haversineDistance(c.lat, c.lon, ep.lat, ep.lon); r > c.radius {
			c.radius = r
		}
	}

	// Rank clusters by dwell time (most dwelt first) for stable auto-labels.
	type ranked struct {
		label int
		c     *clusterAgg
	}
	order := make([]ranked, 0, len(clusters))
	for label, c := range clusters {
		order = append(order, ranked{label, c})
	}
	sort.Slice(order, func(a, b int) bool {
		if order[a].c.dwell != order[b].c.dwell {
			return order[a].c.dwell > order[b].c.dwell
		}
		return order[a].label < order[b].label
	})

	totalDwell := 0.0
	for _, c := range clusters {
		totalDwell += c.dwell
	}

	// Reconcile with stored waypoints so user names survive recomputation:
	// each cluster claims the nearest unclaimed stored waypoint within
	// dwellMatchRadiusM; unmatched clusters get a fresh row.
	stored, err := d.GetWaypoints(trackerID)
	if err != nil {
		return nil, nil, err
	}
	claimed := make(map[int64]bool)

	waypoints := make([]Waypoint, 0, len(order))
	wpIndexByLabel := make(map[int]int, len(order))

	for _, r := range order {
		c := r.c
		var match *WaypointRecord
		best := dwellMatchRadiusM
		for i := range stored {
			if claimed[stored[i].ID] {
				continue
			}
			if dist := haversineDistance(c.lat, c.lon, stored[i].Latitude, stored[i].Longitude); dist < best {
				best = dist
				match = &stored[i]
			}
		}
		if match != nil {
			claimed[match.ID] = true
		}

		// Prune places not visited within the prune horizon: delete the
		// stored row (name included) and drop the cluster from the
		// response. A genuinely revisited place is re-detected as a fresh
		// waypoint.
		if c.lastVisit.Before(pruneCutoff) {
			if match != nil {
				slog.Debug("pruning unvisited waypoint", "id", match.ID, "tracker_id", trackerID, "last_visit", c.lastVisit)
				if err := d.DeleteWaypoint(match.ID); err != nil {
					return nil, nil, err
				}
			}
			continue
		}

		rank := len(waypoints)
		wpIndexByLabel[r.label] = rank
		var wp Waypoint
		if match != nil {
			if err := d.UpdateWaypointLocation(match.ID, c.lat, c.lon); err != nil {
				return nil, nil, err
			}
			wp.ID = match.ID
			if match.Name != nil && *match.Name != "" {
				wp.Name = *match.Name
				wp.Named = true
			}
		} else {
			id, err := d.CreateWaypoint(trackerID, c.lat, c.lon)
			if err != nil {
				return nil, nil, err
			}
			wp.ID = id
		}
		if wp.Name == "" {
			wp.Name = fmt.Sprintf("Waypoint %d", rank+1)
		}

		wp.Latitude = c.lat
		wp.Longitude = c.lon
		wp.RadiusM = c.radius
		wp.DwellSeconds = c.dwell
		if totalDwell > 0 {
			wp.Share = c.dwell / totalDwell
		}
		wp.Visits = c.visits
		wp.Hours = c.hours
		wp.LastVisit = c.lastVisit

		waypoints = append(waypoints, wp)
	}

	// Prune stored waypoints no cluster claimed and with no dwell episode
	// near them inside the prune horizon — covers analysis windows shorter
	// than the horizon, where a stale place produces no cluster at all.
	pruneEpisodes := detectDwellEpisodes(gated)
	for i := range stored {
		if claimed[stored[i].ID] {
			continue
		}
		var lastVisit time.Time
		for _, ep := range pruneEpisodes {
			if ep.end.After(lastVisit) &&
				haversineDistance(stored[i].Latitude, stored[i].Longitude, ep.lat, ep.lon) <= dwellMatchRadiusM {
				lastVisit = ep.end
			}
		}
		if lastVisit.Before(pruneCutoff) {
			slog.Debug("pruning unvisited waypoint", "id", stored[i].ID, "tracker_id", trackerID, "last_visit", lastVisit)
			if err := d.DeleteWaypoint(stored[i].ID); err != nil {
				return nil, nil, err
			}
		}
	}

	// Dwell in the previous window, attributed to the nearest current
	// waypoint within the match radius (previous-period clustering could
	// produce different clusters; matching against current ones keeps the
	// trend comparable).
	prevEpisodes := detectDwellEpisodes(prev)
	prevTotal := 0.0
	prevDwell := make(map[int64]float64)
	for _, ep := range prevEpisodes {
		dur := ep.end.Sub(ep.start).Seconds()
		prevTotal += dur
		var bestID int64
		best := dwellMatchRadiusM
		for i := range waypoints {
			if dist := haversineDistance(ep.lat, ep.lon, waypoints[i].Latitude, waypoints[i].Longitude); dist < best {
				best = dist
				bestID = waypoints[i].ID
			}
		}
		if bestID != 0 {
			prevDwell[bestID] += dur
		}
	}
	for i := range waypoints {
		if prevTotal > 0 {
			waypoints[i].PrevShare = prevDwell[waypoints[i].ID] / prevTotal
		}
	}

	// Transitions between consecutive episodes in different clusters.
	counts := map[[2]int64]int{}
	outgoing := map[int64]int{}
	for i := 0; i+1 < len(episodes); i++ {
		fromIdx, fromOK := wpIndexByLabel[labels[i]]
		toIdx, toOK := wpIndexByLabel[labels[i+1]]
		if !fromOK || !toOK {
			continue
		}
		from, to := waypoints[fromIdx].ID, waypoints[toIdx].ID
		if from == to {
			continue
		}
		counts[[2]int64{from, to}]++
		outgoing[from]++
	}
	transitions := make([]WaypointTransition, 0, len(counts))
	for edge, count := range counts {
		transitions = append(transitions, WaypointTransition{
			From:        edge[0],
			To:          edge[1],
			Count:       count,
			Probability: float64(count) / float64(outgoing[edge[0]]),
		})
	}
	sort.Slice(transitions, func(a, b int) bool {
		if transitions[a].From != transitions[b].From {
			return transitions[a].From < transitions[b].From
		}
		return transitions[a].To < transitions[b].To
	})

	return waypoints, transitions, nil
}

// waypointNameValid reports whether a user-supplied waypoint name is storable.
func waypointNameValid(name string) bool {
	return len(name) <= 100
}
