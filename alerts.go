package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"
)

// Trapped-cat / out-of-territory alerting (GitHub issue #7).
//
// A transparent heuristic over the outputs of the other analytics, evaluated
// after each successful sync (SyncWorker.SyncAll / SyncTracker) and persisted
// to the `alerts` table — the table is the event log. Active alert state is
// always derived from rows with cleared_at IS NULL, so evaluation is
// idempotent across restarts and repeated runs.
//
// "Possibly trapped" fires when, over the trailing trappedWindow:
//   - the gated GPS fixes (FixGPS only — a FixHome fix means the cat is home)
//     span essentially the whole window and all lie within
//     trappedStationaryFactor x the median fix radius of their centroid
//     (effectively stationary), and
//   - the latest zone-known fix does not place the cat in the home WiFi zone
//     (presence.go's zoneState), and
//   - the stationary spot is not a known dwell waypoint: dwell episodes are
//     recomputed from history ending at the start of the current stationary
//     run (walked back with dwell.go's continuity rule), so the ongoing stay
//     itself can never define its own waypoint and suppress the alert.
//
// "Out of territory" fires when the last territoryConsecutiveFixes gated GPS
// fixes are all more than territoryMinOutsideM outside the 95% home-range
// contour (kde.go) computed over the trailing territoryRefDays ending
// territoryRefLag before now — the lag keeps the candidate fixes out of their
// own reference polygon. No contour (too little training data) means no alert.
//
// Alerts clear automatically at the next evaluation where the condition no
// longer holds. When the newest fix of any kind is older than trappedStaleFix
// the data is too stale to judge either way and the alert state is left
// untouched (a silent collar is not evidence of movement, nor of stillness).

const (
	// Alert kinds stored in alerts.kind.
	alertKindTrapped        = "trapped"
	alertKindOutOfTerritory = "out_of_territory"
)

const (
	// trappedWindow: the cat must be effectively stationary for at least this
	// long before a "trapped" alert fires (issue suggests 3-4h).
	trappedWindow = 3*time.Hour + 30*time.Minute
	// trappedCoverageSlack: the stationary run may start this much later than
	// the window start and still count as covering the window.
	trappedCoverageSlack = 45 * time.Minute
	// trappedStaleFix: newest fix older than this disables evaluation (both
	// kinds) — no raising and no clearing on stale data.
	trappedStaleFix = time.Hour
	// trappedMinFixes: minimum gated GPS fixes in the window; a handful of
	// jittery fixes is not enough evidence.
	trappedMinFixes = 5
	// trappedStationaryFactor: "effectively stationary" spread limit, as a
	// multiple of the median fix accuracy radius.
	trappedStationaryFactor = 2.0
	// territoryRefDays / territoryRefLag: the 95% home-range contour is
	// computed over this trailing window, ending this long before now so the
	// candidate fixes don't stretch their own reference polygon.
	territoryRefDays = 30
	territoryRefLag  = time.Hour
	// territoryMinOutsideM / territoryConsecutiveFixes: this many consecutive
	// gated fixes must each be at least this far outside the home-range
	// polygon before an "out of territory" alert fires.
	territoryMinOutsideM      = 100.0
	territoryConsecutiveFixes = 3
	// territoryRecentDays: how far back to look for the last gated fixes.
	territoryRecentDays = 7
	// waypointHistoryDays: dwell history depth for the known-waypoint check.
	waypointHistoryDays = 30
	// alertHistoryLimit: row cap for the history endpoint.
	alertHistoryLimit = 200
)

// AlertRecord is one row of the alerts event log.
type AlertRecord struct {
	ID        int64
	TrackerID int
	Name      string // tracker name, filled by the list queries
	Kind      string
	StartedAt time.Time
	ClearedAt *time.Time
	LastLat   float64
	LastLon   float64
	Radius    *int
	Details   string
}

// alertEvidence carries what a firing condition observed, for the alert row.
type alertEvidence struct {
	start   time.Time // when the condition began (first contributing fix)
	lat     float64
	lon     float64
	radius  *int
	details string
}

// scanAlert scans one alert row (with the tracker-name join) into a record.
func scanAlert(row interface{ Scan(...interface{}) error }) (*AlertRecord, error) {
	var a AlertRecord
	var clearedAt sql.NullTime
	var radius sql.NullInt64
	var details sql.NullString
	err := row.Scan(&a.ID, &a.TrackerID, &a.Name, &a.Kind, &a.StartedAt,
		&clearedAt, &a.LastLat, &a.LastLon, &radius, &details)
	if err != nil {
		return nil, err
	}
	if clearedAt.Valid {
		a.ClearedAt = &clearedAt.Time
	}
	if radius.Valid {
		r := int(radius.Int64)
		a.Radius = &r
	}
	a.Details = details.String
	return &a, nil
}

const alertSelect = `
	SELECT a.id, a.tracker_id, t.name, a.kind, a.started_at, a.cleared_at,
	       a.last_lat, a.last_lon, a.radius, a.details
	FROM alerts a
	JOIN trackers t ON t.id = a.tracker_id
`

// GetActiveAlert returns the active alert of a kind for a tracker
// (nil, nil when none). At most one active alert per tracker+kind is ever
// kept, so no ORDER BY/LIMIT is needed.
func (d *Database) GetActiveAlert(trackerID int, kind string) (*AlertRecord, error) {
	row := d.db.QueryRow(alertSelect+`
		WHERE a.tracker_id = ? AND a.kind = ? AND a.cleared_at IS NULL
	`, trackerID, kind)
	a, err := scanAlert(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// GetActiveAlerts returns all active alerts across trackers, oldest first.
func (d *Database) GetActiveAlerts() ([]AlertRecord, error) {
	rows, err := d.db.Query(alertSelect + `
		WHERE a.cleared_at IS NULL
		ORDER BY a.started_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []AlertRecord
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, *a)
	}
	return alerts, rows.Err()
}

// GetAlertHistory returns the alert event log since a given time, newest
// first, capped at alertHistoryLimit rows.
func (d *Database) GetAlertHistory(since time.Time) ([]AlertRecord, error) {
	rows, err := d.db.Query(alertSelect+`
		WHERE a.started_at >= ?
		ORDER BY a.started_at DESC
		LIMIT ?
	`, since, alertHistoryLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []AlertRecord
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, *a)
	}
	return alerts, rows.Err()
}

// InsertAlert opens a new alert and returns its ID.
func (d *Database) InsertAlert(a *AlertRecord) (int64, error) {
	res, err := d.db.Exec(`
		INSERT INTO alerts (tracker_id, kind, started_at, last_lat, last_lon, radius, details)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, a.TrackerID, a.Kind, a.StartedAt, a.LastLat, a.LastLon, a.Radius, a.Details)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TouchAlert refreshes an active alert's last-fix position and details.
func (d *Database) TouchAlert(id int64, lat, lon float64, radius *int, details string) error {
	_, err := d.db.Exec(`
		UPDATE alerts SET last_lat = ?, last_lon = ?, radius = ?, details = ?
		WHERE id = ?
	`, lat, lon, radius, details, id)
	return err
}

// ClearAlert closes an active alert.
func (d *Database) ClearAlert(id int64, at time.Time) error {
	_, err := d.db.Exec(`UPDATE alerts SET cleared_at = ? WHERE id = ?`, at, id)
	return err
}

// evaluateAlerts runs the alert heuristics for every tracker after a sync.
// Failures are logged, never propagated: alerting must not fail the sync.
func (w *SyncWorker) evaluateAlerts() {
	trackers, err := w.db.GetAllTrackers()
	if err != nil {
		w.logger.Error("Failed to list trackers for alert evaluation", "error", err)
		return
	}
	for _, t := range trackers {
		if err := w.db.evaluateTrackerAlerts(t.ID, w.logger); err != nil {
			w.logger.Error("Alert evaluation failed", "tracker_id", t.ID, "error", err)
		}
	}
}

// evaluateTrackerAlerts evaluates both heuristics for one tracker against the
// current database state and reconciles the alerts table: raise when firing
// and no active alert exists, refresh position while firing, clear when the
// condition stopped. Stale data (no fix within trappedStaleFix) leaves the
// alert state untouched.
func (d *Database) evaluateTrackerAlerts(trackerID int, logger *slog.Logger) error {
	now := time.Now()
	windowStart := now.Add(-trappedWindow)

	fixes, err := d.GetPositionsForRange(trackerID, windowStart, time.Time{})
	if err != nil {
		return err
	}
	if len(fixes) == 0 || now.Sub(fixes[len(fixes)-1].Timestamp) > trappedStaleFix {
		return nil // no recent data: nothing can be concluded either way
	}

	// --- possibly trapped ---
	trapped, ev := assessTrapped(fixes, now)
	if trapped {
		presFixes, err := d.GetPositionsForPresence(trackerID, windowStart)
		if err != nil {
			return err
		}
		if latestZoneStateHome(presFixes) {
			trapped = false
		}
	}
	if trapped {
		// Find when the current continuous stationary run actually began (it
		// may predate the window) — that is both the alert's start time and
		// the cutoff for the waypoint history, so the ongoing stay can never
		// define its own waypoint and suppress the alert.
		longFixes, err := d.GetPositionsForRange(trackerID, now.AddDate(0, 0, -waypointHistoryDays).Add(-trappedWindow), time.Time{})
		if err != nil {
			return err
		}
		runStart := stationaryRunStart(longFixes, windowStart)
		ev.start = runStart
		if atKnownWaypoint(longFixes, ev.lat, ev.lon, runStart) {
			trapped = false
		}
	}
	if err := d.applyAlertState(trackerID, alertKindTrapped, trapped, ev, now, logger); err != nil {
		return err
	}

	// --- out of territory ---
	outside, ev, err := d.assessOutOfTerritory(trackerID, now)
	if err != nil {
		return err
	}
	return d.applyAlertState(trackerID, alertKindOutOfTerritory, outside, ev, now, logger)
}

// applyAlertState reconciles one alert kind with the observed condition.
func (d *Database) applyAlertState(trackerID int, kind string, firing bool, ev alertEvidence, now time.Time, logger *slog.Logger) error {
	active, err := d.GetActiveAlert(trackerID, kind)
	if err != nil {
		return err
	}

	if firing {
		if active == nil {
			id, err := d.InsertAlert(&AlertRecord{
				TrackerID: trackerID,
				Kind:      kind,
				StartedAt: ev.start,
				LastLat:   ev.lat,
				LastLon:   ev.lon,
				Radius:    ev.radius,
				Details:   ev.details,
			})
			if err != nil {
				return err
			}
			logger.Warn("Alert raised", "alert_id", id, "tracker_id", trackerID, "kind", kind,
				"started_at", ev.start, "details", ev.details)
			return nil
		}
		// Still firing: refresh the locate position, keep the original start.
		return d.TouchAlert(active.ID, ev.lat, ev.lon, ev.radius, ev.details)
	}

	if active != nil {
		if err := d.ClearAlert(active.ID, now); err != nil {
			return err
		}
		logger.Info("Alert cleared", "alert_id", active.ID, "tracker_id", trackerID, "kind", kind,
			"duration", now.Sub(active.StartedAt).Round(time.Second))
	}
	return nil
}

// assessTrapped reports whether the gated fixes of the trailing window show a
// cat effectively stationary for the whole window, and where. Window fixes
// must be time-ordered (GetPositionsForRange's are).
func assessTrapped(fixes []DwellPosition, now time.Time) (bool, alertEvidence) {
	var gps []DwellPosition
	for _, f := range fixes {
		if ClassifyFix(f.ValidSignal, f.Type) == FixGPS {
			gps = append(gps, f)
		}
	}
	if len(gps) < trappedMinFixes {
		return false, alertEvidence{}
	}

	first, last := gps[0], gps[len(gps)-1]
	if now.Sub(last.Timestamp) > trappedStaleFix {
		return false, alertEvidence{}
	}
	// The stationary run must cover (almost) the whole window: otherwise the
	// cat only recently stopped, which is ordinary behaviour.
	if last.Timestamp.Sub(first.Timestamp) < trappedWindow-trappedCoverageSlack {
		return false, alertEvidence{}
	}

	medianRadius := medianFixRadius(gps)

	var sumLat, sumLon float64
	for _, f := range gps {
		sumLat += f.Latitude
		sumLon += f.Longitude
	}
	cLat, cLon := sumLat/float64(len(gps)), sumLon/float64(len(gps))

	spread := 0.0
	for _, f := range gps {
		spread = math.Max(spread, haversineDistance(cLat, cLon, f.Latitude, f.Longitude))
	}
	if spread > trappedStationaryFactor*medianRadius {
		return false, alertEvidence{}
	}

	return true, alertEvidence{
		start:  first.Timestamp,
		lat:    last.Latitude,
		lon:    last.Longitude,
		radius: last.Radius,
		details: fmt.Sprintf(`{"spread_m":%.1f,"median_radius_m":%.1f,"fixes":%d}`,
			spread, medianRadius, len(gps)),
	}
}

// medianFixRadius is the median reported accuracy radius of the fixes,
// falling back to kdeDefaultRadiusM when no fix reports one.
func medianFixRadius(fixes []DwellPosition) float64 {
	var radii []float64
	for _, f := range fixes {
		if f.Radius != nil && *f.Radius > 0 {
			radii = append(radii, float64(*f.Radius))
		}
	}
	if len(radii) == 0 {
		return kdeDefaultRadiusM
	}
	sort.Float64s(radii)
	return radii[len(radii)/2]
}

// latestZoneStateHome reports whether the most recent zone-known fix places
// the cat in the home WiFi zone. Fixes without zone data are skipped (they
// carry no presence evidence, same rule as presence.go).
func latestZoneStateHome(fixes []PresencePosition) bool {
	for i := len(fixes) - 1; i >= 0; i-- {
		if state, known := zoneState(fixes[i]); known {
			return state == PresenceHome
		}
	}
	return false
}

// stationaryRunStart walks back from the first in-window gated fix over the
// earlier gated fixes while they stay within dwellStationaryRadiusM of it
// with gaps under dwellMaxGapS — the same continuity rule as dwell episode
// detection — and returns when the current continuous stationary run began.
// fixes must be time-ordered and cover at least [windowStart, now].
func stationaryRunStart(fixes []DwellPosition, windowStart time.Time) time.Time {
	var gps []DwellPosition
	for _, f := range fixes {
		if ClassifyFix(f.ValidSignal, f.Type) == FixGPS {
			gps = append(gps, f)
		}
	}

	idx := -1
	for i, f := range gps {
		if !f.Timestamp.Before(windowStart) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return windowStart
	}

	anchor := gps[idx]
	start := anchor.Timestamp
	prev := anchor.Timestamp
	for j := idx - 1; j >= 0; j-- {
		if prev.Sub(gps[j].Timestamp).Seconds() > dwellMaxGapS {
			break
		}
		if haversineDistance(anchor.Latitude, anchor.Longitude, gps[j].Latitude, gps[j].Longitude) > dwellStationaryRadiusM {
			break
		}
		start = gps[j].Timestamp
		prev = gps[j].Timestamp
	}
	return start
}

// atKnownWaypoint reports whether (lat, lon) lies within a known dwell
// waypoint, computed from the fixes before `until` (the start of the current
// stationary run — see stationaryRunStart). Fixes must be time-ordered.
func atKnownWaypoint(fixes []DwellPosition, lat, lon float64, until time.Time) bool {
	var gated []DwellPosition
	for _, f := range fixes {
		if f.Timestamp.Before(until) && dwellGate(f) {
			gated = append(gated, f)
		}
	}
	episodes := detectDwellEpisodes(gated)
	if len(episodes) == 0 {
		return false
	}
	labels := dbscanEpisodes(episodes, dwellClusterEpsilonM)

	// Aggregate clusters like computeWaypoints: centroid plus the furthest
	// episode centroid as the waypoint's reach.
	type agg struct {
		sumLat, sumLon float64
		visits         int
		lat, lon       float64
		radius         float64
	}
	clusters := map[int]*agg{}
	for i, ep := range episodes {
		c := clusters[labels[i]]
		if c == nil {
			c = &agg{}
			clusters[labels[i]] = c
		}
		c.sumLat += ep.lat
		c.sumLon += ep.lon
		c.visits++
	}
	for _, c := range clusters {
		c.lat = c.sumLat / float64(c.visits)
		c.lon = c.sumLon / float64(c.visits)
	}
	for i, ep := range episodes {
		c := clusters[labels[i]]
		c.radius = math.Max(c.radius, haversineDistance(c.lat, c.lon, ep.lat, ep.lon))
	}

	for _, c := range clusters {
		if haversineDistance(lat, lon, c.lat, c.lon) <= c.radius+dwellMatchRadiusM {
			return true
		}
	}
	return false
}

// assessOutOfTerritory reports whether the last territoryConsecutiveFixes
// gated GPS fixes all lie more than territoryMinOutsideM outside the 95%
// home-range contour of the reference window.
func (d *Database) assessOutOfTerritory(trackerID int, now time.Time) (bool, alertEvidence, error) {
	est, err := d.computeRange(trackerID, now.AddDate(0, 0, -territoryRefDays), now.Add(-territoryRefLag))
	if err != nil {
		return false, alertEvidence{}, err
	}
	if len(est.Home) == 0 {
		// Too little training data for a contour: no reference, no alert.
		return false, alertEvidence{}, nil
	}

	recent, err := d.GetPositionsForRange(trackerID, now.AddDate(0, 0, -territoryRecentDays), time.Time{})
	if err != nil {
		return false, alertEvidence{}, err
	}
	var gps []DwellPosition
	for _, f := range recent {
		if ClassifyFix(f.ValidSignal, f.Type) == FixGPS {
			gps = append(gps, f)
		}
	}
	if len(gps) < territoryConsecutiveFixes {
		return false, alertEvidence{}, nil
	}
	tail := gps[len(gps)-territoryConsecutiveFixes:]
	if now.Sub(tail[len(tail)-1].Timestamp) > trappedStaleFix {
		return false, alertEvidence{}, nil
	}

	minOutside := math.Inf(1)
	for _, f := range tail {
		dist := distanceOutsideRings(est.Home, f.Latitude, f.Longitude)
		if dist <= territoryMinOutsideM {
			return false, alertEvidence{}, nil
		}
		minOutside = math.Min(minOutside, dist)
	}

	last := tail[len(tail)-1]
	return true, alertEvidence{
		start:  tail[0].Timestamp,
		lat:    last.Latitude,
		lon:    last.Longitude,
		radius: last.Radius,
		details: fmt.Sprintf(`{"outside_m":%.1f,"consecutive_fixes":%d}`,
			minOutside, territoryConsecutiveFixes),
	}, nil
}

// distanceOutsideRings returns the distance in meters from (lat, lon) to the
// nearest home-range ring boundary, or 0 when the point is inside any ring.
// Rings are GeoJSON-style [lon, lat] loops; geometry is computed in a local
// equirectangular projection around the point (kde.go's localProjection).
func distanceOutsideRings(rings [][][2]float64, lat, lon float64) float64 {
	proj := newLocalProjection(lat, lon) // the point itself sits at the origin

	best := math.Inf(1)
	for _, ring := range rings {
		pts := make([]msPoint, len(ring))
		for i, c := range ring {
			x, y := proj.toXY(c[1], c[0])
			pts[i] = msPoint{x, y}
		}
		if pointInRing(pts) {
			return 0
		}
		best = math.Min(best, ringDistance(pts))
	}
	return best
}

// pointInRing ray-casts the origin against a ring of projected points.
func pointInRing(pts []msPoint) bool {
	inside := false
	j := len(pts) - 1
	for i := range pts {
		yi, yj := pts[i].y, pts[j].y
		if (yi > 0) != (yj > 0) && 0 < (pts[j].x-pts[i].x)*(0-yi)/(yj-yi)+pts[i].x {
			inside = !inside
		}
		j = i
	}
	return inside
}

// ringDistance is the minimum distance from the origin to a ring's boundary.
func ringDistance(pts []msPoint) float64 {
	best := math.Inf(1)
	for i := range pts {
		j := (i + 1) % len(pts)
		best = math.Min(best, originSegmentDistance(pts[i], pts[j]))
	}
	return best
}

// originSegmentDistance is the distance from the origin to segment a-b.
func originSegmentDistance(a, b msPoint) float64 {
	dx, dy := b.x-a.x, b.y-a.y
	lenSq := dx*dx + dy*dy
	if lenSq == 0 {
		return math.Hypot(a.x, a.y)
	}
	t := -(a.x*dx + a.y*dy) / lenSq
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(a.x+t*dx, a.y+t*dy)
}
