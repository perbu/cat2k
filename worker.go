package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	weenect "github.com/perbu/weenect-go"
)

// ErrSyncInProgress is returned by TrySyncAll when a sync is already running.
var ErrSyncInProgress = errors.New("sync already in progress")

// SyncWorker handles synchronization of tracker data
type SyncWorker struct {
	client      *weenect.Client
	db          *Database
	rateLimiter *RateLimiter
	logger      *slog.Logger
	cfg         *Config

	// syncMu serializes syncs within the daemon so a manual (HTTP-triggered)
	// sync and a scheduled sync never hit the Weenect client concurrently.
	syncMu sync.Mutex
}

// TrySyncAll runs SyncAll only if no other sync is currently running. If one is
// already in progress it returns ErrSyncInProgress immediately rather than
// queueing, so callers (the scheduler and the manual /api/sync endpoint) can
// skip or report the conflict instead of piling up redundant syncs.
func (w *SyncWorker) TrySyncAll(ctx context.Context) error {
	if !w.syncMu.TryLock() {
		return ErrSyncInProgress
	}
	defer w.syncMu.Unlock()
	return w.SyncAll(ctx)
}

// newSyncWorker creates a new sync worker
func newSyncWorker(cfg *Config, db *Database, logger *slog.Logger) *SyncWorker {
	client := weenect.NewClient(cfg.Username, cfg.Password)
	rateLimiter := newRateLimiter(cfg.RateLimit, logger)

	return &SyncWorker{
		client:      client,
		db:          db,
		rateLimiter: rateLimiter,
		logger:      logger,
		cfg:         cfg,
	}
}

// SyncAll syncs all trackers
func (w *SyncWorker) SyncAll(ctx context.Context) error {
	w.logger.Info("Starting sync for all trackers")
	startTime := time.Now()

	// Login to Weenect
	if err := w.rateLimiter.Wait(ctx); err != nil {
		return err
	}
	w.logger.Debug("API request: login")
	if err := w.client.Login(ctx); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	w.logger.Debug("API response: login successful")

	// Get all trackers
	if err := w.rateLimiter.Wait(ctx); err != nil {
		return err
	}
	w.logger.Debug("API request: get trackers")
	trackers, err := w.client.GetTrackers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get trackers: %w", err)
	}
	w.logger.Debug("API response: got trackers", "count", len(trackers.Items))

	w.logger.Info("Found trackers", "count", len(trackers.Items))

	totalPositions := 0
	successCount := 0
	errorCount := 0

	// Sync each tracker
	for _, tracker := range trackers.Items {
		// Update tracker in database
		if err := w.db.UpsertTracker(tracker.ID, tracker.Name); err != nil {
			w.logger.Error("Failed to upsert tracker", "tracker_id", tracker.ID, "error", err)
			errorCount++
			continue
		}

		// Sync tracker positions
		positions, err := w.syncTracker(ctx, tracker.ID)
		if err != nil {
			w.logger.Error("Failed to sync tracker", "tracker_id", tracker.ID, "error", err)
			errorCount++
			continue
		}

		totalPositions += positions
		successCount++
		w.logger.Info("Synced tracker", "tracker_id", tracker.ID, "name", tracker.Name, "positions", positions)
	}

	// Evaluate trapped / out-of-territory alerts against the fresh data
	// (issue #7). Failures are logged inside, never fail the sync.
	w.evaluateAlerts()

	duration := time.Since(startTime)

	// Log overall sync
	logEntry := &SyncLogRecord{
		SyncTime:         time.Now(),
		PositionsFetched: totalPositions,
		Success:          errorCount == 0,
		DurationMs:       int(duration.Milliseconds()),
	}
	if errorCount > 0 {
		errMsg := fmt.Sprintf("%d trackers failed", errorCount)
		logEntry.ErrorMessage = &errMsg
	}

	if err := w.db.InsertSyncLog(logEntry); err != nil {
		w.logger.Error("Failed to log sync", "error", err)
	}

	w.logger.Info("Sync completed",
		"duration", duration,
		"success", successCount,
		"errors", errorCount,
		"positions", totalPositions,
	)

	if errorCount > 0 {
		return fmt.Errorf("sync completed with %d errors", errorCount)
	}

	return nil
}

// SyncTracker syncs a specific tracker
func (w *SyncWorker) SyncTracker(ctx context.Context, trackerID int) error {
	w.logger.Info("Starting sync for tracker", "tracker_id", trackerID)
	startTime := time.Now()

	// Login
	if err := w.rateLimiter.Wait(ctx); err != nil {
		return err
	}
	w.logger.Debug("API request: login")
	if err := w.client.Login(ctx); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	w.logger.Debug("API response: login successful")

	positions, err := w.syncTracker(ctx, trackerID)

	duration := time.Since(startTime)

	// Log sync
	logEntry := &SyncLogRecord{
		TrackerID:        &trackerID,
		SyncTime:         time.Now(),
		PositionsFetched: positions,
		Success:          err == nil,
		DurationMs:       int(duration.Milliseconds()),
	}
	if err != nil {
		errMsg := err.Error()
		logEntry.ErrorMessage = &errMsg
	}

	if logErr := w.db.InsertSyncLog(logEntry); logErr != nil {
		w.logger.Error("Failed to log sync", "error", logErr)
	}

	if err != nil {
		return err
	}

	// Evaluate alerts for this tracker against the fresh data (issue #7).
	if err := w.db.evaluateTrackerAlerts(trackerID, w.logger); err != nil {
		w.logger.Error("Alert evaluation failed", "tracker_id", trackerID, "error", err)
	}

	w.logger.Info("Sync completed", "tracker_id", trackerID, "positions", positions, "duration", duration)
	return nil
}

// syncTracker performs the actual sync for a tracker (internal)
func (w *SyncWorker) syncTracker(ctx context.Context, trackerID int) (int, error) {
	// Get tracker from database to find last sync time
	tracker, err := w.db.GetTracker(trackerID)
	if err != nil {
		// Tracker doesn't exist yet, use backfill start date
		w.logger.Debug("Tracker not in database, will use backfill date", "tracker_id", trackerID)
	}

	// Determine start date
	startDate := time.Now().AddDate(0, 0, -1) // Default: yesterday
	if tracker != nil && !tracker.LastSyncTimestamp.IsZero() {
		// Use last sync time
		startDate = tracker.LastSyncTimestamp
	} else if w.cfg.BackfillStartDate != "" {
		// Use configured backfill date
		if parsed, err := time.Parse("2006-01-02", w.cfg.BackfillStartDate); err == nil {
			startDate = parsed
		}
	}

	endDate := time.Now()

	return w.fetchAndStorePositions(ctx, trackerID, startDate, endDate)
}

// BackfillAll backfills all trackers
func (w *SyncWorker) BackfillAll(ctx context.Context, startDate, endDate time.Time) error {
	w.logger.Info("Starting backfill for all trackers", "start", startDate, "end", endDate)

	// Login
	if err := w.rateLimiter.Wait(ctx); err != nil {
		return err
	}
	w.logger.Debug("API request: login")
	if err := w.client.Login(ctx); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	w.logger.Debug("API response: login successful")

	// Get all trackers
	if err := w.rateLimiter.Wait(ctx); err != nil {
		return err
	}
	w.logger.Debug("API request: get trackers")
	trackers, err := w.client.GetTrackers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get trackers: %w", err)
	}
	w.logger.Debug("API response: got trackers", "count", len(trackers.Items))

	for _, tracker := range trackers.Items {
		if err := w.db.UpsertTracker(tracker.ID, tracker.Name); err != nil {
			w.logger.Error("Failed to upsert tracker", "tracker_id", tracker.ID, "error", err)
			continue
		}

		positions, err := w.fetchAndStorePositions(ctx, tracker.ID, startDate, endDate)
		if err != nil {
			w.logger.Error("Failed to backfill tracker", "tracker_id", tracker.ID, "error", err)
			continue
		}

		w.logger.Info("Backfilled tracker", "tracker_id", tracker.ID, "positions", positions)
	}

	return nil
}

// BackfillTracker backfills a specific tracker
func (w *SyncWorker) BackfillTracker(ctx context.Context, trackerID int, startDate, endDate time.Time) error {
	w.logger.Info("Starting backfill for tracker", "tracker_id", trackerID, "start", startDate, "end", endDate)

	// Login
	if err := w.rateLimiter.Wait(ctx); err != nil {
		return err
	}
	w.logger.Debug("API request: login")
	if err := w.client.Login(ctx); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	w.logger.Debug("API response: login successful")

	positions, err := w.fetchAndStorePositions(ctx, trackerID, startDate, endDate)
	if err != nil {
		return err
	}

	w.logger.Info("Backfill completed", "tracker_id", trackerID, "positions", positions)
	return nil
}

// fetchAndStorePositions fetches and stores positions for a tracker
// Splits large date ranges into 24-hour chunks since the API has a 24h limit
func (w *SyncWorker) fetchAndStorePositions(ctx context.Context, trackerID int, startDate, endDate time.Time) (int, error) {
	w.logger.Debug("Fetching positions",
		"tracker_id", trackerID,
		"start", startDate.Format("2006-01-02 15:04:05"),
		"end", endDate.Format("2006-01-02 15:04:05"),
	)

	// API has 24-hour limit, so split into chunks if needed
	const maxDuration = 24 * time.Hour
	totalPositions := 0
	currentStart := startDate

	for currentStart.Before(endDate) {
		// Calculate chunk end (24 hours from start, or final end date)
		chunkEnd := currentStart.Add(maxDuration)
		if chunkEnd.After(endDate) {
			chunkEnd = endDate
		}

		w.logger.Debug("Fetching chunk",
			"tracker_id", trackerID,
			"chunk_start", currentStart.Format("2006-01-02 15:04:05"),
			"chunk_end", chunkEnd.Format("2006-01-02 15:04:05"),
		)

		// Rate limit
		if err := w.rateLimiter.Wait(ctx); err != nil {
			return totalPositions, err
		}

		// Fetch positions for this chunk
		w.logger.Debug("API request: get positions",
			"tracker_id", trackerID,
			"start", currentStart.Format("2006-01-02 15:04:05"),
			"end", chunkEnd.Format("2006-01-02 15:04:05"))
		positions, err := w.client.GetPosition(ctx, trackerID, &currentStart, &chunkEnd)
		if err != nil {
			return totalPositions, fmt.Errorf("failed to get positions: %w", err)
		}
		w.logger.Debug("API response: got positions", "count", len(positions))

		// Store positions for this chunk
		for _, pos := range positions {
			// Convert WeenectTime to *time.Time for database
			var lastMessage, dateServer, dateTracker *time.Time
			if !pos.LastMessage.IsZero() {
				t := pos.LastMessage.Time
				lastMessage = &t
			}
			if !pos.DateServer.IsZero() {
				t := pos.DateServer.Time
				dateServer = &t
			}
			if !pos.DateTracker.IsZero() {
				t := pos.DateTracker.Time
				dateTracker = &t
			}

			// Convert non-pointer fields to pointers for database
			battery := pos.Battery
			direction := pos.Direction
			validSignal := pos.ValidSignal
			satellites := pos.Satellites
			gsm := pos.GSM
			typ := pos.Type

			// Extended per-fix quality and presence fields (issue #1)
			radius := pos.Radius
			wifiZoneID := pos.WifiZoneID
			wifiZoneIDExited := pos.WifiZoneIDExited
			sensorStatus := pos.SensorStatus
			isInDeepSleep := pos.IsInDeepSleep
			gsmState := pos.GSMState
			accuracyState := pos.AccuracyState
			batteryState := pos.BatteryState

			record := &PositionRecord{
				ID:          pos.ID,
				TrackerID:   trackerID,
				Timestamp:   pos.GetTimestamp(),
				Latitude:    pos.Latitude,
				Longitude:   pos.Longitude,
				Battery:     &battery,
				Speed:       pos.Speed,
				Direction:   &direction,
				ValidSignal: &validSignal,
				Satellites:  &satellites,
				GSM:         &gsm,
				Type:        &typ,
				LastMessage: lastMessage,
				DateServer:  dateServer,
				DateTracker: dateTracker,

				Radius:           &radius,
				PDOP:             pos.PDOP,
				WifiZoneID:       &wifiZoneID,
				WifiZoneIDExited: &wifiZoneIDExited,
				SensorStatus:     &sensorStatus,
				IsInDeepSleep:    &isInDeepSleep,
				GeofenceName:     pos.GeofenceName,
				GSMState:         &gsmState,
				AccuracyState:    &accuracyState,
				BatteryState:     &batteryState,
				OffReason:        pos.OffReason,
			}

			if err := w.db.InsertPosition(record); err != nil {
				return totalPositions, fmt.Errorf("failed to insert position: %w", err)
			}
		}

		totalPositions += len(positions)

		// Update sync time after each successful chunk for incremental progress
		// This ensures we don't lose progress if interrupted mid-sync
		if err := w.db.UpdateTrackerSyncTime(trackerID, chunkEnd); err != nil {
			return totalPositions, fmt.Errorf("failed to update sync time: %w", err)
		}

		w.logger.Debug("Updated sync timestamp",
			"tracker_id", trackerID,
			"sync_time", chunkEnd.Format("2006-01-02 15:04:05"))

		// Move to next chunk
		currentStart = chunkEnd
	}

	return totalPositions, nil
}
