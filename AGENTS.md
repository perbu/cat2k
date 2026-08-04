# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Catboard 2000 (cat2k) is a Go daemon that syncs GPS position data from Weenect trackers to a local SQLite database. It runs on a schedule (configurable, default: every 5 minutes) and supports manual syncs and historical data backfill.

The daemon also provides a web-based radar status page that shows:
- Real-time tracker positions on a radar display (relative to home location)
- Pet inside/outside status from SureHub pet flaps
- Configurable points of interest (POIs)

## Build and Run

```bash
# Build the binary
go build -o cat2k

# Run daemon with scheduled syncs
./cat2k run

# Manual sync
./cat2k sync-now

# Backfill historical data
./cat2k backfill --start-date 2024-01-01

# View status
./cat2k status

# View statistics
./cat2k stats
```

## Configuration

Configuration is loaded from (in priority order):
1. Environment variables (highest)
2. Config file from default locations: `./config.json`, `./cat2k.json`, `~/.config/cat2k/config.json`, `~/.cat2k/config.json`
3. Command-line defaults

Required configuration:
- `WEENECT_USERNAME` or `username` in config.json
- `WEENECT_PASSWORD` or `password` in config.json

Optional configuration:
- `home_lat`, `home_lon` - Home coordinates for radar center point
- `surehub_email`, `surehub_password` - SureHub credentials for pet flap status
- `pois` - Array of points of interest `[{"name": "Place", "lat": 59.0, "lon": 10.0, "color": "#fff"}]`
- `sync_schedule` - Cron expression (default: `*/5 * * * *` for every 5 minutes)
- `http_listen` - HTTP server listen address (default: `:8080`, e.g., `127.0.0.1:8080` for localhost only)

Copy `config.example.json` to `config.json` to get started.

## Architecture

The daemon consists of several key components:

### main.go
Entry point with command routing. Commands: `run`, `sync-now`, `backfill`, `status`, `stats`, `version`.

### config.go
Configuration loading from files and environment variables. Uses `loadConfig()` which searches default paths automatically. Also defines the `POI` struct for points of interest.

### worker.go (SyncWorker)
Core sync logic that:
- Authenticates with Weenect API using github.com/perbu/weenect-go client
- Fetches tracker list and position data
- Splits large date ranges into 24-hour chunks (API limitation)
- Stores positions idempotently in SQLite
- Updates `last_sync_timestamp` after each successful chunk for resumability

Key methods:
- `SyncAll(ctx)` - Sync all trackers incrementally from last sync time
- `SyncTracker(ctx, trackerID)` - Sync specific tracker
- `BackfillAll(ctx, start, end)` - Backfill all trackers for date range
- `BackfillTracker(ctx, trackerID, start, end)` - Backfill specific tracker
- `fetchAndStorePositions()` - Internal method that handles 24h chunking

### database.go
SQLite operations using modernc.org/sqlite driver. Three tables:
- `trackers` - Tracker metadata with `last_sync_timestamp`
- `positions` - GPS positions (idempotent inserts by position ID)
- `sync_log` - Sync operation history

Key methods:
- `GetLatestPositions()` - Returns most recent position per tracker (used by status page)

All position inserts use `ON CONFLICT(id) DO NOTHING` for idempotency.

### scheduler.go
Cron-based scheduling using github.com/robfig/cron/v3. Runs sync jobs on configured schedule with graceful shutdown support.

### ratelimit.go
Token bucket rate limiter to respect API limits (default: 4 req/sec).

### logger.go
Structured logging using slog. Debug level logs all API requests/responses and rate limiter activity.

### api.go
HTTP API server providing:
- `GET /api/trackers` - List all trackers with position counts
- `GET /api/positions/{id}` - Get positions for a tracker (with date range query params)
- `GET /api/status` - Radar status endpoint returning latest positions, home coords, POIs, and SureHub pet status
- `GET /health` - Health check endpoint
- Static file serving from `./web` directory

The `/api/status` endpoint integrates with SureHub (github.com/perbu/go-sure) to fetch pet inside/outside status. Results are cached for 5 minutes to avoid excessive API calls.

### web/status.html
Radar-style status page showing:
- Trackers positioned relative to home on a 1km radius radar
- Distance and bearing to each tracker
- Battery percentage
- Inside/outside status from SureHub pet flaps
- Configurable POIs as markers
- Auto-refreshes every 30 seconds

## Key Design Patterns

**Idempotency**: Position records use API-provided IDs as primary keys with `ON CONFLICT DO NOTHING`, allowing safe re-syncing without duplicates.

**Resumability**: Each tracker stores `last_sync_timestamp`. Syncs are incremental from this timestamp. Updated after each successful 24h chunk, so interrupted syncs can resume.

**24-hour chunking**: API has 24h max range. `fetchAndStorePositions()` automatically splits larger ranges and updates sync time after each chunk.

**Rate limiting**: All API calls go through `rateLimiter.Wait()` before execution.

## Testing Changes

Since this is a daemon, test by:
1. Building: `go build`
2. Running manual sync: `./cat2k sync-now` (requires valid credentials)
3. Checking database: `sqlite3 catboard.db "SELECT COUNT(*) FROM positions"`
4. Enabling debug logs: `export WEENECT_LOG_LEVEL=debug`

## Dependencies

- github.com/perbu/weenect-go - Weenect API client
- github.com/perbu/go-sure - SureHub/SureFlap API client for pet flap status
- github.com/robfig/cron/v3 - Cron scheduler
- modernc.org/sqlite - Pure Go SQLite driver
- Standard library: log/slog, database/sql, context, time, encoding/json
