package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "0.1.0"

// extractConfigFlag extracts the --config flag value from args
func extractConfigFlag(args []string) string {
	for i, arg := range args {
		if arg == "--config" || arg == "-config" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if len(arg) > 9 && arg[:9] == "--config=" {
			return arg[9:]
		}
		if len(arg) > 8 && arg[:8] == "-config=" {
			return arg[8:]
		}
	}
	return ""
}

func run() error {
	if len(os.Args) < 2 {
		printUsage()
		return fmt.Errorf("no command specified")
	}

	command := os.Args[1]

	// Special case for version - no config needed
	if command == "version" {
		fmt.Printf("Catboard 2000 v%s\n", version)
		return nil
	}

	// Extract --config flag value from remaining args
	configPath := extractConfigFlag(os.Args[2:])

	// Load config once
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	switch command {
	case "run":
		return runDaemon(cfg)
	case "sync-now":
		return syncNow(cfg, os.Args[2:])
	case "backfill":
		return backfill(cfg, os.Args[2:])
	case "status":
		return showStatus(cfg)
	case "stats":
		return showStats(cfg, os.Args[2:])
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", command)
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`Catboard 2000 v%s - GPS tracker data collection daemon

Usage:
  cat2k <command> [flags]

Commands:
  run         Start daemon with scheduled syncs
  sync-now    Manual sync now
  backfill    Backfill historical data
  status      Show daemon status and last sync info
  stats       Show statistics
  version     Show version information

Flags:
  Run 'cat2k <command> -h' for command-specific flags

`, version)
}

func runDaemon(cfg *Config) error {
	logger := newLogger(cfg.LogLevel)
	logger.Info("Starting Catboard 2000", "version", version)

	// Initialize database
	db, err := initDatabase(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.Close()

	// Create sync worker
	worker := newSyncWorker(cfg, db, logger)

	// Create scheduler
	scheduler := newScheduler(cfg.SyncSchedule, worker, logger)

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start API server if enabled
	var apiServer *APIServer
	var apiServerErr chan error
	if cfg.HTTPEnabled {
		apiServer = NewAPIServer(db, cfg, cfg.HTTPListen, logger)
		apiServerErr = make(chan error, 1)

		// Push live updates to connected browsers after each successful sync.
		scheduler.onSyncComplete = apiServer.NotifyUpdate

		go func() {
			if err := apiServer.Start(); err != nil {
				apiServerErr <- err
			}
		}()
	}

	// Start scheduler
	schedulerErr := make(chan error, 1)
	go func() {
		schedulerErr <- scheduler.Run(ctx)
	}()

	logger.Info("Daemon started", "schedule", cfg.SyncSchedule, "http_enabled", cfg.HTTPEnabled, "http_listen", cfg.HTTPListen)

	// Wait for shutdown signal or errors
	select {
	case <-sigChan:
		logger.Info("Received shutdown signal")
		cancel()

		// Graceful shutdown of API server
		if apiServer != nil {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := apiServer.Shutdown(shutdownCtx); err != nil {
				logger.Error("API server shutdown error", "error", err)
			}
		}

		// Wait for scheduler to finish
		<-schedulerErr

	case err := <-schedulerErr:
		if err != nil {
			logger.Error("Scheduler error", "error", err)
			if apiServer != nil {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shutdownCancel()
				apiServer.Shutdown(shutdownCtx)
			}
			return fmt.Errorf("scheduler error: %w", err)
		}

	case err := <-apiServerErr:
		logger.Error("API server error", "error", err)
		cancel()
		<-schedulerErr
		return fmt.Errorf("API server error: %w", err)
	}

	logger.Info("Daemon stopped")
	return nil
}

func syncNow(cfg *Config, args []string) error {
	flags := flag.NewFlagSet("sync-now", flag.ExitOnError)
	trackerID := flags.Int("tracker-id", 0, "Sync specific tracker only (default: all)")
	flags.Parse(args)

	logger := newLogger(cfg.LogLevel)

	db, err := initDatabase(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.Close()

	worker := newSyncWorker(cfg, db, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	logger.Info("Starting manual sync")

	var syncErr error
	if *trackerID > 0 {
		syncErr = worker.SyncTracker(ctx, *trackerID)
	} else {
		syncErr = worker.SyncAll(ctx)
	}

	if syncErr != nil {
		return fmt.Errorf("sync failed: %w", syncErr)
	}

	logger.Info("Sync completed successfully")
	return nil
}

func backfill(cfg *Config, args []string) error {
	flags := flag.NewFlagSet("backfill", flag.ExitOnError)
	startDate := flags.String("start-date", "", "Start date for backfill (YYYY-MM-DD)")
	endDate := flags.String("end-date", "", "End date for backfill (YYYY-MM-DD, default: today)")
	trackerID := flags.Int("tracker-id", 0, "Backfill specific tracker only (default: all)")
	flags.Parse(args)

	if *startDate == "" {
		flags.Usage()
		return fmt.Errorf("--start-date is required")
	}

	logger := newLogger(cfg.LogLevel)

	db, err := initDatabase(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.Close()

	start, err := time.Parse("2006-01-02", *startDate)
	if err != nil {
		return fmt.Errorf("invalid start date format: %w", err)
	}

	end := time.Now()
	if *endDate != "" {
		end, err = time.Parse("2006-01-02", *endDate)
		if err != nil {
			return fmt.Errorf("invalid end date format: %w", err)
		}
	}

	worker := newSyncWorker(cfg, db, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()

	logger.Info("Starting backfill", "start", start, "end", end)

	var backfillErr error
	if *trackerID > 0 {
		backfillErr = worker.BackfillTracker(ctx, *trackerID, start, end)
	} else {
		backfillErr = worker.BackfillAll(ctx, start, end)
	}

	if backfillErr != nil {
		return fmt.Errorf("backfill failed: %w", backfillErr)
	}

	logger.Info("Backfill completed successfully")
	return nil
}

func showStatus(cfg *Config) error {
	db, err := initDatabase(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.Close()

	status, err := db.GetStatus()
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	stats, err := db.GetStats(0)
	if err != nil {
		return fmt.Errorf("failed to get tracker stats: %w", err)
	}

	fmt.Printf("Catboard 2000 Status\n")
	fmt.Printf("====================\n\n")
	fmt.Printf("Database: %s\n", cfg.DatabasePath)
	fmt.Printf("Trackers: %d\n", status.TrackerCount)
	fmt.Printf("Total Positions: %d\n", status.PositionCount)

	if len(stats) > 0 {
		fmt.Printf("\nPositions per Tracker:\n")
		for _, s := range stats {
			fmt.Printf("  %s (ID %d): %d positions", s.TrackerName, s.TrackerID, s.PositionCount)
			if !s.LastSync.IsZero() {
				fmt.Printf(" (last sync: %s)", s.LastSync.Format("2006-01-02 15:04"))
			}
			fmt.Printf("\n")
		}
	}

	fmt.Printf("\nLast Sync:\n")
	if status.LastSyncTime.IsZero() {
		fmt.Printf("  Never synced\n")
	} else {
		fmt.Printf("  Time: %s\n", status.LastSyncTime.Format(time.RFC3339))
		fmt.Printf("  Success: %v\n", status.LastSyncSuccess)
		fmt.Printf("  Positions Fetched: %d\n", status.LastSyncPositions)
		if status.LastSyncError != "" {
			fmt.Printf("  Error: %s\n", status.LastSyncError)
		}
	}
	return nil
}

func showStats(cfg *Config, args []string) error {
	flags := flag.NewFlagSet("stats", flag.ExitOnError)
	trackerID := flags.Int("tracker-id", 0, "Show stats for specific tracker (default: all)")
	flags.Parse(args)

	db, err := initDatabase(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.Close()

	stats, err := db.GetStats(*trackerID)
	if err != nil {
		return fmt.Errorf("failed to get stats: %w", err)
	}

	if *trackerID > 0 {
		fmt.Printf("Statistics for Tracker %d\n", *trackerID)
	} else {
		fmt.Printf("Statistics for All Trackers\n")
	}
	fmt.Printf("===========================\n\n")

	for _, s := range stats {
		fmt.Printf("Tracker: %s (ID: %d)\n", s.TrackerName, s.TrackerID)
		fmt.Printf("  Positions: %d\n", s.PositionCount)
		if !s.FirstPosition.IsZero() {
			fmt.Printf("  First Position: %s\n", s.FirstPosition.Format(time.RFC3339))
		}
		if !s.LastPosition.IsZero() {
			fmt.Printf("  Last Position: %s\n", s.LastPosition.Format(time.RFC3339))
		}
		if !s.LastSync.IsZero() {
			fmt.Printf("  Last Sync: %s\n", s.LastSync.Format(time.RFC3339))
		}
		fmt.Println()
	}
	return nil
}