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

	// onSyncComplete, if set, is called after each successful sync. Used to
	// push live updates to connected web clients.
	onSyncComplete func()
}

// newScheduler creates a new scheduler
func newScheduler(schedule string, worker *SyncWorker, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		schedule: schedule,
		worker:   worker,
		logger:   logger,
		cron:     cron.New(),
	}
}

// Run starts the scheduler and blocks until context is cancelled
func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Info("Setting up scheduler", "schedule", s.schedule)

	// Add sync job to cron
	_, err := s.cron.AddFunc(s.schedule, func() {
		s.logger.Info("Scheduled sync triggered")

		// Create context with timeout for sync operation
		syncCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		err := s.worker.TrySyncAll(syncCtx)
		switch {
		case errors.Is(err, ErrSyncInProgress):
			s.logger.Info("Scheduled sync skipped: a sync is already running")
		case err != nil:
			s.logger.Error("Scheduled sync failed", "error", err)
		default:
			s.logger.Info("Scheduled sync completed successfully")
			if s.onSyncComplete != nil {
				s.onSyncComplete()
			}
		}
	})
	if err != nil {
		return err
	}

	// Start cron scheduler
	s.cron.Start()
	s.logger.Info("Scheduler started")

	// Wait for context cancellation
	<-ctx.Done()

	// Graceful shutdown
	s.logger.Info("Stopping scheduler")
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()

	return nil
}