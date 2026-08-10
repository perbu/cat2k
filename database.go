package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Database provides SQLite database operations
type Database struct {
	db *sql.DB
}

// TrackerRecord represents a tracker in the database
type TrackerRecord struct {
	ID                int
	Name              string
	LastSyncTimestamp time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// PositionRecord represents a position in the database
type PositionRecord struct {
	ID          string
	TrackerID   int
	Timestamp   time.Time
	Latitude    float64
	Longitude   float64
	Battery     *int
	Speed       *float64
	Direction   *int
	ValidSignal *bool
	Satellites  *int
	GSM         *int
	Type        *string
	LastMessage *time.Time
	DateServer  *time.Time
	DateTracker *time.Time
	// Extended per-fix quality and presence fields (see GitHub issue #1).
	Radius           *int
	PDOP             *float64
	WifiZoneID       *int
	WifiZoneIDExited *int
	SensorStatus     *string
	IsInDeepSleep    *bool
	GeofenceName     *string
	GSMState         *string
	AccuracyState    *string
	BatteryState     *string
	OffReason        *string
	CreatedAt        time.Time
}

// SyncLogRecord represents a sync log entry
type SyncLogRecord struct {
	ID               int
	TrackerID        *int
	SyncTime         time.Time
	PositionsFetched int
	StartDate        *time.Time
	EndDate          *time.Time
	Success          bool
	ErrorMessage     *string
	DurationMs       int
}

// StatusInfo holds daemon status information
type StatusInfo struct {
	TrackerCount      int
	PositionCount     int
	LastSyncTime      time.Time
	LastSyncSuccess   bool
	LastSyncPositions int
	LastSyncError     string
}

// TrackerStats holds statistics for a tracker
type TrackerStats struct {
	TrackerID     int
	TrackerName   string
	PositionCount int
	FirstPosition time.Time
	LastPosition  time.Time
	LastSync      time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS trackers (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  last_sync_timestamp DATETIME,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS positions (
  id TEXT PRIMARY KEY,
  tracker_id INTEGER NOT NULL,
  timestamp DATETIME NOT NULL,
  latitude REAL NOT NULL,
  longitude REAL NOT NULL,
  battery INTEGER,
  speed REAL,
  direction INTEGER,
  valid_signal BOOLEAN,
  satellites INTEGER,
  gsm INTEGER,
  type TEXT,
  last_message DATETIME,
  date_server DATETIME,
  date_tracker DATETIME,
  radius INTEGER,
  pdop REAL,
  wifi_zone_id INTEGER,
  wifi_zone_id_exited INTEGER,
  sensor_status TEXT,
  is_in_deep_sleep BOOLEAN,
  geofence_name TEXT,
  gsm_state TEXT,
  accuracy_state TEXT,
  battery_state TEXT,
  off_reason TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (tracker_id) REFERENCES trackers(id)
);

CREATE INDEX IF NOT EXISTS idx_positions_tracker_timestamp
  ON positions(tracker_id, timestamp);

-- Supports time-range scans across all trackers (heatmap, activity), where the
-- leading-column-tracker_id index above cannot seek on timestamp alone.
CREATE INDEX IF NOT EXISTS idx_positions_timestamp
  ON positions(timestamp);

CREATE TABLE IF NOT EXISTS sync_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tracker_id INTEGER,
  sync_time DATETIME NOT NULL,
  positions_fetched INTEGER DEFAULT 0,
  start_date DATETIME,
  end_date DATETIME,
  success BOOLEAN NOT NULL,
  error_message TEXT,
  duration_ms INTEGER,
  FOREIGN KEY (tracker_id) REFERENCES trackers(id)
);

-- Dwell-point waypoints (issue #2). Clustering runs on demand; this table
-- only persists waypoint identity (per tracker, matched by centroid
-- proximity) and the user-assigned name so names survive recomputation.
CREATE TABLE IF NOT EXISTS waypoints (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tracker_id INTEGER NOT NULL,
  latitude REAL NOT NULL,
  longitude REAL NOT NULL,
  name TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (tracker_id) REFERENCES trackers(id)
);
`

// ensureDatabaseDir checks that the database file's directory exists, creating
// it if only the leaf is missing. A deeper gap usually means a typo in
// database_path, and SQLite reports that as a misleading "out of memory" error,
// so fail early with the actual path instead.
func ensureDatabaseDir(dbPath string) error {
	if dbPath == ":memory:" {
		return nil
	}

	dir := filepath.Dir(dbPath)
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("database directory %s is not a directory", dir)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat database directory %s: %w", dir, err)
	}

	// Only create the leaf directory: if its parent is missing too, the path is
	// more likely wrong than genuinely uncreated.
	if parent := filepath.Dir(dir); parent != dir {
		if _, err := os.Stat(parent); err != nil {
			return fmt.Errorf("database directory %s does not exist (and neither does %s) - check database_path", dir, parent)
		}
	}

	if err := os.Mkdir(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create database directory %s: %w", dir, err)
	}

	return nil
}

// initDatabase initializes the database with schema
func initDatabase(dbPath string) (*Database, error) {
	if err := ensureDatabaseDir(dbPath); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// sql.Open is lazy, so ping here to attribute open failures to the file
	// rather than to whichever statement happens to run first.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to open database %s: %w", dbPath, err)
	}

	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	// Create schema
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	// CREATE TABLE IF NOT EXISTS does not add columns to databases created
	// before the extended position fields (issue #1) were introduced.
	if err := migratePositionsTable(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate positions table: %w", err)
	}

	return &Database{db: db}, nil
}

// positionColumns lists every column of the positions table added after the
// original schema, with its DDL definition. Fresh databases get these from the
// CREATE TABLE statement; existing databases get them via ALTER TABLE here.
var positionColumns = []struct {
	name string
	ddl  string
}{
	{"radius", "radius INTEGER"},
	{"pdop", "pdop REAL"},
	{"wifi_zone_id", "wifi_zone_id INTEGER"},
	{"wifi_zone_id_exited", "wifi_zone_id_exited INTEGER"},
	{"sensor_status", "sensor_status TEXT"},
	{"is_in_deep_sleep", "is_in_deep_sleep BOOLEAN"},
	{"geofence_name", "geofence_name TEXT"},
	{"gsm_state", "gsm_state TEXT"},
	{"accuracy_state", "accuracy_state TEXT"},
	{"battery_state", "battery_state TEXT"},
	{"off_reason", "off_reason TEXT"},
}

// migratePositionsTable adds any missing extended columns to an existing
// positions table. Idempotent: columns that already exist are skipped.
func migratePositionsTable(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(positions)")
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, col := range positionColumns {
		if existing[col.name] {
			continue
		}
		if _, err := db.Exec("ALTER TABLE positions ADD COLUMN " + col.ddl); err != nil {
			return fmt.Errorf("failed to add column %s: %w", col.name, err)
		}
	}

	return nil
}

// Close closes the database connection
func (d *Database) Close() error {
	return d.db.Close()
}

// UpsertTracker inserts or updates a tracker
func (d *Database) UpsertTracker(id int, name string) error {
	query := `
		INSERT INTO trackers (id, name, created_at, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			updated_at = CURRENT_TIMESTAMP
	`
	_, err := d.db.Exec(query, id, name)
	return err
}

// GetTracker retrieves a tracker by ID
func (d *Database) GetTracker(id int) (*TrackerRecord, error) {
	query := `
		SELECT id, name, last_sync_timestamp, created_at, updated_at
		FROM trackers WHERE id = ?
	`
	var t TrackerRecord
	var lastSync sql.NullTime

	err := d.db.QueryRow(query, id).Scan(
		&t.ID, &t.Name, &lastSync, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	if lastSync.Valid {
		t.LastSyncTimestamp = lastSync.Time
	}

	return &t, nil
}

// UpdateTrackerSyncTime updates the last sync timestamp for a tracker
func (d *Database) UpdateTrackerSyncTime(id int, syncTime time.Time) error {
	query := `
		UPDATE trackers
		SET last_sync_timestamp = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`
	_, err := d.db.Exec(query, syncTime, id)
	return err
}

// InsertPosition inserts a position (idempotent by position ID). On conflict
// the extended quality/presence columns are updated so a re-backfill fills
// them in for historical rows; position IDs are stable, so this stays
// idempotent.
func (d *Database) InsertPosition(p *PositionRecord) error {
	query := `
		INSERT INTO positions (
			id, tracker_id, timestamp, latitude, longitude,
			battery, speed, direction, valid_signal, satellites,
			gsm, type, last_message, date_server, date_tracker,
			radius, pdop, wifi_zone_id, wifi_zone_id_exited,
			sensor_status, is_in_deep_sleep, geofence_name,
			gsm_state, accuracy_state, battery_state, off_reason,
			created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			radius = excluded.radius,
			pdop = excluded.pdop,
			wifi_zone_id = excluded.wifi_zone_id,
			wifi_zone_id_exited = excluded.wifi_zone_id_exited,
			sensor_status = excluded.sensor_status,
			is_in_deep_sleep = excluded.is_in_deep_sleep,
			geofence_name = excluded.geofence_name,
			gsm_state = excluded.gsm_state,
			accuracy_state = excluded.accuracy_state,
			battery_state = excluded.battery_state,
			off_reason = excluded.off_reason
	`
	_, err := d.db.Exec(query,
		p.ID, p.TrackerID, p.Timestamp, p.Latitude, p.Longitude,
		p.Battery, p.Speed, p.Direction, p.ValidSignal, p.Satellites,
		p.GSM, p.Type, p.LastMessage, p.DateServer, p.DateTracker,
		p.Radius, p.PDOP, p.WifiZoneID, p.WifiZoneIDExited,
		p.SensorStatus, p.IsInDeepSleep, p.GeofenceName,
		p.GSMState, p.AccuracyState, p.BatteryState, p.OffReason,
	)
	return err
}

// InsertSyncLog logs a sync operation
func (d *Database) InsertSyncLog(log *SyncLogRecord) error {
	query := `
		INSERT INTO sync_log (
			tracker_id, sync_time, positions_fetched,
			start_date, end_date, success, error_message, duration_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := d.db.Exec(query,
		log.TrackerID, log.SyncTime, log.PositionsFetched,
		log.StartDate, log.EndDate, log.Success, log.ErrorMessage, log.DurationMs,
	)
	return err
}

// GetStatus returns overall daemon status
func (d *Database) GetStatus() (*StatusInfo, error) {
	var status StatusInfo

	// Get tracker count
	err := d.db.QueryRow("SELECT COUNT(*) FROM trackers").Scan(&status.TrackerCount)
	if err != nil {
		return nil, err
	}

	// Get position count
	err = d.db.QueryRow("SELECT COUNT(*) FROM positions").Scan(&status.PositionCount)
	if err != nil {
		return nil, err
	}

	// Get last sync info
	query := `
		SELECT sync_time, success, positions_fetched, error_message
		FROM sync_log
		ORDER BY sync_time DESC
		LIMIT 1
	`
	var syncTime sql.NullTime
	var errorMsg sql.NullString
	err = d.db.QueryRow(query).Scan(
		&syncTime, &status.LastSyncSuccess, &status.LastSyncPositions, &errorMsg,
	)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	if syncTime.Valid {
		status.LastSyncTime = syncTime.Time
	}
	if errorMsg.Valid {
		status.LastSyncError = errorMsg.String
	}

	return &status, nil
}

// GetStats returns statistics for trackers
func (d *Database) GetStats(trackerID int) ([]TrackerStats, error) {
	query := `
		SELECT
			t.id,
			t.name,
			COUNT(p.id) as position_count,
			MIN(p.timestamp) as first_position,
			MAX(p.timestamp) as last_position,
			t.last_sync_timestamp
		FROM trackers t
		LEFT JOIN positions p ON t.id = p.tracker_id
	`

	args := []interface{}{}
	if trackerID > 0 {
		query += " WHERE t.id = ?"
		args = append(args, trackerID)
	}

	query += " GROUP BY t.id ORDER BY t.name"

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []TrackerStats
	for rows.Next() {
		var s TrackerStats
		var firstPos, lastPos, lastSync sql.NullTime

		err := rows.Scan(
			&s.TrackerID, &s.TrackerName, &s.PositionCount,
			&firstPos, &lastPos, &lastSync,
		)
		if err != nil {
			return nil, err
		}

		if firstPos.Valid {
			s.FirstPosition = firstPos.Time
		}
		if lastPos.Valid {
			s.LastPosition = lastPos.Time
		}
		if lastSync.Valid {
			s.LastSync = lastSync.Time
		}

		stats = append(stats, s)
	}

	return stats, rows.Err()
}

// TrackerWithCount represents a tracker with position count for API responses
type TrackerWithCount struct {
	ID            int       `json:"id"`
	Name          string    `json:"name"`
	LastSync      time.Time `json:"last_sync"`
	PositionCount int       `json:"position_count"`
}

// GetAllTrackers retrieves all trackers with position counts
func (d *Database) GetAllTrackers() ([]TrackerWithCount, error) {
	query := `
		SELECT
			t.id,
			t.name,
			t.last_sync_timestamp,
			COUNT(p.id) as position_count
		FROM trackers t
		LEFT JOIN positions p ON t.id = p.tracker_id
		GROUP BY t.id
		ORDER BY t.name
	`

	rows, err := d.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trackers []TrackerWithCount
	for rows.Next() {
		var t TrackerWithCount
		var lastSync sql.NullTime
		err := rows.Scan(&t.ID, &t.Name, &lastSync, &t.PositionCount)
		if err != nil {
			return nil, err
		}
		if lastSync.Valid {
			t.LastSync = lastSync.Time
		}
		trackers = append(trackers, t)
	}

	return trackers, rows.Err()
}

// SimplePosition represents a simplified position for API responses
type SimplePosition struct {
	Latitude  float64   `json:"lat"`
	Longitude float64   `json:"lng"`
	Timestamp time.Time `json:"timestamp"`
	Battery   *int      `json:"battery,omitempty"`
}

// GetPositions retrieves positions for a tracker within a time range
func (d *Database) GetPositions(trackerID int, start, end time.Time) ([]SimplePosition, error) {
	query := `
		SELECT latitude, longitude, timestamp, battery
		FROM positions
		WHERE tracker_id = ? AND timestamp >= ? AND timestamp <= ?
		ORDER BY timestamp DESC
		LIMIT 10000
	`

	rows, err := d.db.Query(query, trackerID, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []SimplePosition
	for rows.Next() {
		var p SimplePosition
		err := rows.Scan(&p.Latitude, &p.Longitude, &p.Timestamp, &p.Battery)
		if err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}

	return positions, rows.Err()
}

// TrackerExists checks if a tracker exists
func (d *Database) TrackerExists(trackerID int) (bool, error) {
	var exists bool
	err := d.db.QueryRow("SELECT EXISTS(SELECT 1 FROM trackers WHERE id = ?)", trackerID).Scan(&exists)
	return exists, err
}

// LatestPosition represents the most recent position for a tracker
type LatestPosition struct {
	TrackerID   int       `json:"tracker_id"`
	TrackerName string    `json:"name"`
	Latitude    float64   `json:"lat"`
	Longitude   float64   `json:"lon"`
	Timestamp   time.Time `json:"timestamp"`
	Battery     *int      `json:"battery,omitempty"`
}

// GetRecentPositions returns positions for a tracker within a time window
func (d *Database) GetRecentPositions(trackerID int, since time.Time) ([]SimplePosition, error) {
	query := `
		SELECT latitude, longitude, timestamp, battery
		FROM positions
		WHERE tracker_id = ? AND timestamp >= ?
		ORDER BY timestamp ASC
	`

	rows, err := d.db.Query(query, trackerID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []SimplePosition
	for rows.Next() {
		var p SimplePosition
		err := rows.Scan(&p.Latitude, &p.Longitude, &p.Timestamp, &p.Battery)
		if err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}

	return positions, rows.Err()
}

// HeatmapPosition represents a position with just coordinates for heatmap generation
type HeatmapPosition struct {
	TrackerID int
	Latitude  float64
	Longitude float64
}

// GetPositionsForHeatmap returns all positions for heatmap generation since a given time
// Returns a map of tracker_id -> slice of positions
func (d *Database) GetPositionsForHeatmap(since time.Time) (map[int][]HeatmapPosition, error) {
	query := `
		SELECT tracker_id, latitude, longitude
		FROM positions
		WHERE timestamp >= ?
		ORDER BY tracker_id
	`

	rows, err := d.db.Query(query, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int][]HeatmapPosition)
	for rows.Next() {
		var p HeatmapPosition
		err := rows.Scan(&p.TrackerID, &p.Latitude, &p.Longitude)
		if err != nil {
			return nil, err
		}
		result[p.TrackerID] = append(result[p.TrackerID], p)
	}

	return result, rows.Err()
}

// GetPositionsForActivity returns gated-eligible positions per tracker since a
// given time, ordered by tracker and timestamp. Includes the quality fields
// (satellites, valid_signal) needed to filter bad fixes during measurement.
func (d *Database) GetPositionsForActivity(since time.Time) (map[int][]ActivityPosition, error) {
	query := `
		SELECT tracker_id, timestamp, latitude, longitude, satellites, valid_signal, type
		FROM positions
		WHERE timestamp >= ?
		ORDER BY timestamp
	`

	rows, err := d.db.Query(query, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int][]ActivityPosition)
	for rows.Next() {
		var p ActivityPosition
		var sats sql.NullInt64
		var valid sql.NullBool
		var typ sql.NullString
		if err := rows.Scan(&p.TrackerID, &p.Timestamp, &p.Latitude, &p.Longitude, &sats, &valid, &typ); err != nil {
			return nil, err
		}
		if sats.Valid {
			s := int(sats.Int64)
			p.Satellites = &s
		}
		if valid.Valid {
			v := valid.Bool
			p.ValidSignal = &v
		}
		if typ.Valid {
			t := typ.String
			p.Type = &t
		}
		result[p.TrackerID] = append(result[p.TrackerID], p)
	}

	return result, rows.Err()
}

// GetLatestPositions returns the most recent position for each tracker
func (d *Database) GetLatestPositions() ([]LatestPosition, error) {
	query := `
		SELECT t.id, t.name, p.latitude, p.longitude, p.timestamp, p.battery
		FROM trackers t
		INNER JOIN positions p ON t.id = p.tracker_id
		WHERE p.timestamp = (
			SELECT MAX(p2.timestamp)
			FROM positions p2
			WHERE p2.tracker_id = t.id
		)
		ORDER BY t.name
	`

	rows, err := d.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []LatestPosition
	for rows.Next() {
		var p LatestPosition
		err := rows.Scan(&p.TrackerID, &p.TrackerName, &p.Latitude, &p.Longitude, &p.Timestamp, &p.Battery)
		if err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}

	return positions, rows.Err()
}

// WaypointRecord is a stored dwell-point waypoint (issue #2): the persistent
// identity of a detected cluster, plus its user-assigned name.
type WaypointRecord struct {
	ID        int64
	TrackerID int
	Latitude  float64
	Longitude float64
	Name      *string
}

// GetWaypoints returns all stored waypoints for a tracker.
func (d *Database) GetWaypoints(trackerID int) ([]WaypointRecord, error) {
	query := `
		SELECT id, tracker_id, latitude, longitude, name
		FROM waypoints
		WHERE tracker_id = ?
		ORDER BY id
	`

	rows, err := d.db.Query(query, trackerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var waypoints []WaypointRecord
	for rows.Next() {
		var w WaypointRecord
		if err := rows.Scan(&w.ID, &w.TrackerID, &w.Latitude, &w.Longitude, &w.Name); err != nil {
			return nil, err
		}
		waypoints = append(waypoints, w)
	}

	return waypoints, rows.Err()
}

// GetWaypoint retrieves a single waypoint by ID (nil, nil when absent).
func (d *Database) GetWaypoint(id int64) (*WaypointRecord, error) {
	query := `
		SELECT id, tracker_id, latitude, longitude, name
		FROM waypoints
		WHERE id = ?
	`
	var w WaypointRecord
	err := d.db.QueryRow(query, id).Scan(&w.ID, &w.TrackerID, &w.Latitude, &w.Longitude, &w.Name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// CreateWaypoint stores a newly detected waypoint and returns its ID.
func (d *Database) CreateWaypoint(trackerID int, lat, lon float64) (int64, error) {
	query := `
		INSERT INTO waypoints (tracker_id, latitude, longitude, created_at, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`
	res, err := d.db.Exec(query, trackerID, lat, lon)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateWaypointLocation refreshes a stored waypoint's centroid after a
// recomputation matched it to a (slightly drifted) cluster.
func (d *Database) UpdateWaypointLocation(id int64, lat, lon float64) error {
	query := `
		UPDATE waypoints
		SET latitude = ?, longitude = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`
	_, err := d.db.Exec(query, lat, lon, id)
	return err
}

// SetWaypointName assigns or clears (nil name) a waypoint's user name.
func (d *Database) SetWaypointName(id int64, name *string) error {
	query := `
		UPDATE waypoints
		SET name = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`
	_, err := d.db.Exec(query, name, id)
	return err
}

// GetPositionsForDwell returns gate-eligible fields for a tracker since a
// given time, ordered by timestamp, for dwell analysis (issue #2). Follows
// GetPositionsForActivity's pattern; gating itself happens in dwell.go.
func (d *Database) GetPositionsForDwell(trackerID int, since time.Time) ([]DwellPosition, error) {
	return d.GetPositionsForRange(trackerID, since, time.Time{})
}

// GetPositionsForRange returns the same gate-eligible fields as
// GetPositionsForDwell but bounded to [start, end) for monthly KDE windows
// (issue #3). A zero end means "no upper bound". Gating happens in kde.go.
func (d *Database) GetPositionsForRange(trackerID int, start, end time.Time) ([]DwellPosition, error) {
	query := `
		SELECT tracker_id, timestamp, latitude, longitude, valid_signal, type, radius
		FROM positions
		WHERE tracker_id = ? AND timestamp >= ?
	`
	args := []interface{}{trackerID, start}
	if !end.IsZero() {
		query += " AND timestamp < ?"
		args = append(args, end)
	}
	query += " ORDER BY timestamp"

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []DwellPosition
	for rows.Next() {
		var p DwellPosition
		var valid sql.NullBool
		var typ sql.NullString
		var radius sql.NullInt64
		if err := rows.Scan(&p.TrackerID, &p.Timestamp, &p.Latitude, &p.Longitude, &valid, &typ, &radius); err != nil {
			return nil, err
		}
		if valid.Valid {
			v := valid.Bool
			p.ValidSignal = &v
		}
		if typ.Valid {
			t := typ.String
			p.Type = &t
		}
		if radius.Valid {
			r := int(radius.Int64)
			p.Radius = &r
		}
		positions = append(positions, p)
	}

	return positions, rows.Err()
}
