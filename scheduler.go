package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler manages scheduled sync operations
type Scheduler struct {
	schedule string
	worker   *SyncWorker
	logger   *slog.Logger
	cron     *cron.Cron

	// stateInterval is how often the collars' power state is polled, on its
	// own ticker rather than the sync cron: whether a tracker is switched on
	// is a now question, and the cron is coarse (nightly by default). Zero
	// disables the poller.
	stateInterval time.Duration

	// activeSyncInterval is how often to sync while viewerCount reports at
	// least one connected browser, tighter than the idle cron schedule so the
	// web UI stays fresh while someone is actually watching. Zero (or a nil
	// viewerCount) disables this and leaves syncing to the cron alone.
	activeSyncInterval time.Duration

	// viewerCount, if set, reports how many browsers currently have the web
	// UI open (live /api/events SSE connections). Set by main.go to the
	// APIServer's count once the HTTP server exists.
	viewerCount func() int

	// onSyncComplete, if set, is called after each successful sync. Used to
	// push live updates to connected web clients.
	onSyncComplete func()

	// onStateChange, if set, is called when a collar's power state actually
	// changes, so a browser learns a collar came off without waiting for the
	// next position sync.
	onStateChange func()
}

// newScheduler creates a new scheduler
func newScheduler(schedule string, stateInterval, activeSyncInterval time.Duration, worker *SyncWorker, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		schedule:           schedule,
		stateInterval:      stateInterval,
		activeSyncInterval: activeSyncInterval,
		worker:             worker,
		logger:             logger,
		cron:               cron.New(),
	}
}

// pollTrackerStates polls the collars' power state once and fires
// onStateChange if any of them moved. Errors are logged, never fatal: a
// failed poll leaves the last known state in place, which the API serves with
// its own timestamp so a stalled poller cannot masquerade as fresh data.
func (s *Scheduler) pollTrackerStates(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	changed, err := s.worker.SyncTrackerStates(pollCtx)
	switch {
	case err != nil:
		s.logger.Error("Tracker state poll failed", "error", err)
	case changed && s.onStateChange != nil:
		s.onStateChange()
	}
}

// runStatePoller polls tracker power state immediately, then on
// stateInterval, until the context is cancelled. The immediate poll means a
// freshly started daemon does not serve "unknown" for a whole interval.
func (s *Scheduler) runStatePoller(ctx context.Context) {
	s.pollTrackerStates(ctx)

	ticker := time.NewTicker(s.stateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollTrackerStates(ctx)
		}
	}
}

// runSync triggers a sync via TrySyncAll, logging the outcome under the given
// reason ("scheduled" or "active") and firing onSyncComplete on success.
// Shared by the cron job and the active-sync ticker so both report and notify
// identically.
func (s *Scheduler) runSync(reason string) {
	s.logger.Info(reason + " sync triggered")

	syncCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	err := s.worker.TrySyncAll(syncCtx)
	switch {
	case errors.Is(err, ErrSyncInProgress):
		s.logger.Info(reason + " sync skipped: a sync is already running")
	case err != nil:
		s.logger.Error(reason+" sync failed", "error", err)
	default:
		s.logger.Info(reason + " sync completed successfully")
		if s.onSyncComplete != nil {
			s.onSyncComplete()
		}
	}
}

// runActiveSyncer syncs on activeSyncInterval for as long as viewerCount
// reports at least one connected browser, until the context is cancelled. It
// is a plain ticker, not a cron: the idle cron job already covers the
// no-viewer case, so this only needs to add extra syncs while someone is
// watching.
func (s *Scheduler) runActiveSyncer(ctx context.Context) {
	ticker := time.NewTicker(s.activeSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.viewerCount() > 0 {
				s.runSync("active")
			}
		}
	}
}

// Run starts the scheduler and blocks until context is cancelled
func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Info("Setting up scheduler", "schedule", s.schedule)

	// Add sync job to cron
	_, err := s.cron.AddFunc(s.schedule, func() {
		s.runSync("scheduled")
	})
	if err != nil {
		return err
	}

	// Start cron scheduler
	s.cron.Start()
	s.logger.Info("Scheduler started")

	// Poll collar power state on its own cadence alongside the cron.
	if s.stateInterval > 0 {
		s.logger.Info("Starting tracker state poller", "interval", s.stateInterval)
		go s.runStatePoller(ctx)
	}

	// Sync more often while someone has the web UI open, alongside the cron.
	if s.activeSyncInterval > 0 && s.viewerCount != nil {
		s.logger.Info("Starting active-viewer syncer", "interval", s.activeSyncInterval)
		go s.runActiveSyncer(ctx)
	}

	// Wait for context cancellation
	<-ctx.Done()

	// Graceful shutdown
	s.logger.Info("Stopping scheduler")
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()

	return nil
}
