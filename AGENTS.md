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
SQLite operations using modernc.org/sqlite driver. Four tables:
- `trackers` - Tracker metadata with `last_sync_timestamp`
- `positions` - GPS positions (idempotent inserts by position ID)
- `sync_log` - Sync operation history
- `waypoints` - Dwell-point waypoint identity + user-assigned names (issue #2); clustering itself is recomputed on demand, this table only makes names stick

Key methods:
- `GetLatestPositions()` - Returns most recent position per tracker (used by status page)
- `GetPositionsForDwell()` - Gate-eligible fields for a tracker over a time range, ordered by timestamp
- `GetPositionsForRange()` - Same gate-eligible fields bounded to [start, end) for monthly KDE windows (GetPositionsForDwell delegates to it)

All position inserts use `ON CONFLICT(id) DO UPDATE` on the extended quality/presence columns (radius, pdop, wifi_zone_id, …) so a re-backfill backfills them into historical rows; the insert stays idempotent because position IDs are stable. A migration in initDatabase adds the columns to pre-existing databases.

### gate.go
Shared fix-quality gate (`ClassifyFix`): fixes are `FixInvalid` (no valid signal), `FixHome` (snapped CMD-X WiFi-zone fixes — "at home" evidence only), or `FixGPS` (usable for movement analytics). All analytics must pass fixes through this gate; `FixHome` coordinates are snapped to the zone center and fabricate stillness.

### dwell.go
Dwell-point waypoints and territory transition graph (issue #2):
- `detectDwellEpisodes()` groups consecutive gated GPS fixes within 30 m of an anchor into stationary episodes (min 3 min span, max 20 min fix gap)
- `dbscanEpisodes()` clusters episode centroids (epsilon 20 m, minPoints 1, pure Go, O(n²))
- `computeWaypoints()` aggregates clusters (dwell time, visits, hour-of-day histogram, last visit, share + previous-period share) and reconciles them with the `waypoints` table by centroid proximity (40 m match radius) so user names survive recomputation; unnamed waypoints get rank-based auto-labels ("Waypoint 1" = most dwelt)
- Transitions between consecutive episodes in different clusters form the directed territory graph

### kde.go
KDE utilization distributions — 50% core range vs 95% home range contours (issue #3):
- Gated `FixGPS` fixes only, weighted by `FixWeight(radius)`, projected to a local equirectangular plane (meters) around the data centroid; deterministic stride subsampling caps fixes at 20k per evaluation
- Weighted Gaussian KDE on a 128×128 grid over the fix bounding box padded by 3 bandwidths; bandwidth = 2× median fix `radius` clamped to 10–50 m; evaluation only visits cells within 3 bandwidths of each fix
- `isoplethThreshold()` finds the density where cumulative mass reaches 50%/95% (cells sorted by density descending); `contourRings()` extracts the isopleths with a compact marching-squares, chains segments into rings, simplifies with Douglas-Peucker, and emits GeoJSON-style `[lon,lat]` rings
- `overlapIndex()` is the Bhattacharyya affinity between two cats' KDEs on a shared grid (`sum(sqrt(m1·m2))` over per-cell probability masses: 1 = identical, 0 = disjoint), computed per month for the overlap chart

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
- `GET /api/activity` - Per-cat daily activity summary
- `GET /api/waypoints?tracker_id=&days=` - Detected dwell-point waypoints with centroid, dwell time, share (+ previous period), visits, hour-of-day histogram, last visit, and name (stored or auto-label)
- `GET /api/waypoints/transitions?tracker_id=&days=` - Directed territory-graph edges `{from, to, count, probability}`
- `PUT /api/waypoints/{id}` - Assign or clear a waypoint's user name (JSON body `{"name": "..."}`)
- `GET /api/range?tracker_id=&month=YYYY-MM` (or `from`/`to` RFC3339; default rolling 30d) - KDE 50% core and 95% home-range contour rings with `bandwidth_m` and `fixes_used`
- `GET /api/range/overlap?months=12` - Monthly cat-vs-cat Bhattacharyya overlap series `[{month, index, fixes_a, fixes_b}]` (index null when either cat lacks fixes)
- `POST /api/sync` - Manual upstream resync
- `GET /api/events` - SSE stream notifying browsers when fresh data lands
- `GET /health` - Health check endpoint
- Static file serving from `./web` directory

The `/api/status` endpoint integrates with SureHub (github.com/perbu/go-sure) to fetch pet inside/outside status. Results are cached for 5 minutes to avoid excessive API calls.

### web/index.html + web/app.js
Leaflet map view: per-cat heatmaps, recent-movement trails, activity cat cards,
plus the waypoint layer (markers sized by dwell share, per-cat accent colors,
click for stats popup with rename), the toggleable transition-graph overlay
(curved directed edges, width ∝ probability), and a "Top places" sidebar panel
ranking waypoints by dwell share with a trend vs. the previous period. The
"Home range" card adds the KDE contour layer (50% core = stronger fill, 95%
home range = outline, per-cat accent colors and toggles) with a month picker,
and the "Territory overlap" card charts the monthly overlap index — clicking a
bar steps the contour layer to that month.

### web/status.html
Radar-style status page showing:
- Trackers positioned relative to home on a 1km radius radar
- Distance and bearing to each tracker
- Battery percentage
- Inside/outside status from SureHub pet flaps
- Configurable POIs as markers
- Auto-refreshes every 30 seconds

## Key Design Patterns

**Idempotency**: Position records use API-provided IDs as primary keys. Inserts are `ON CONFLICT(id) DO UPDATE` on the extended columns only, so re-syncing never duplicates and re-backfills refresh quality fields.

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
