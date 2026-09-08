# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Catboard 2000 (cat2k) is a Go daemon that syncs GPS position data from Weenect trackers to a local SQLite database. It runs on a schedule (configurable cron; code default nightly at 2am, example config every 5 minutes) and supports manual syncs and historical data backfill.

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
- `sync_schedule` - Cron expression (code default: `0 2 * * *` nightly at 2am; `config.example.json` ships `*/5 * * * *`)
- `tracker_state_interval_s` - How often (seconds, default 60) to poll collar on/off state; 0 disables the poller (see power.go)
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
- Fetches tracker list and position data. The tracker list goes through `fetchTrackerStates()`, not `Client.GetTrackers()`: both hit `GET /mytracker`, but the client's typed `Tracker` keeps only id+name and drops the collar power fields (see power.go)
- Splits large date ranges into 24-hour chunks (API limitation)
- Stores positions idempotently in SQLite
- Updates `last_sync_timestamp` after each successful chunk for resumability
- Evaluates trapped / out-of-territory alerts after each successful sync (issue #7, see alerts.go); evaluation failures are logged, never fail the sync

Key methods:
- `SyncTrackerStates(ctx)` - Poll only the collars' on/off state, returning whether any changed. Unlike `TrySyncAll` it does not take `syncMu`, so a poll never blocks a scheduled sync (which would cost a whole cron period); the two touch disjoint state — power columns vs. positions/`last_sync_timestamp` — and the Weenect client and rate limiter are goroutine-safe
- `SyncAll(ctx)` - Sync all trackers incrementally from last sync time
- `SyncTracker(ctx, trackerID)` - Sync specific tracker
- `BackfillAll(ctx, start, end)` - Backfill all trackers for date range
- `BackfillTracker(ctx, trackerID, start, end)` - Backfill specific tracker
- `fetchAndStorePositions()` - Internal method that handles 24h chunking

### database.go
SQLite operations using modernc.org/sqlite driver. Six tables:
- `trackers` - Tracker metadata with `last_sync_timestamp` and the polled collar on/off state in the `power_*` columns (power.go)
- `positions` - GPS positions (idempotent inserts by position ID)
- `sync_log` - Sync operation history
- `waypoints` - Dwell-point waypoint identity + user-assigned names (issue #2); clustering itself is recomputed on demand, this table only makes names stick; rows with no visit in 10 days are pruned (name included)
- `behavior_models` - Fitted HMM parameters per tracker as a JSON blob (issue #5); refit on demand when stale so request-time decoding never refits
- `alerts` - Alert event log (issue #7); one row per raised alert, `cleared_at IS NULL` marks the (single, per tracker+kind) active alert. Evaluation after each sync reconciles conditions against this table, so alert state survives restarts and the table doubles as the alert history

Key methods:
- `GetLatestPositions()` - Returns most recent position per tracker (used by status page)
- `GetPositionsForDwell()` - Gate-eligible fields for a tracker over a time range, ordered by timestamp
- `GetPositionsForRange()` - Same gate-eligible fields bounded to [start, end) for monthly KDE windows (GetPositionsForDwell delegates to it)

All position inserts use `ON CONFLICT(id) DO UPDATE` on the extended quality/presence columns (radius, pdop, wifi_zone_id, …) so a re-backfill backfills them into historical rows; the insert stays idempotent because position IDs are stable. A migration in initDatabase adds the columns to pre-existing databases.

### power.go
Collar on/off state (`DevicePower`) — is the tracker actually switched on:
- A gap in the position stream cannot answer this. The collars come off most evenings, which looks identical to a lost GSM signal or a cat simply sitting still. The authoritative fields live on Weenect's `GET /mytracker` payload — `position[0].is_online`, `off_reason`, `is_in_deep_sleep` — none of which weenect-go's `Tracker` type decodes, so `weenectTrackerState` decodes the raw body instead (`Client.DoRawRequest`)
- States: `live`, `off` (Weenect named a cause in `off_reason`, e.g. `ALM-OFF` for the power button), `offline` (unreachable, no cause given), `stale` (Weenect still calls it online but it has not reported for `powerStaleAfter` — the collar on a charger indoors; kept distinct from `offline`, which claims unreachability), `sleeping` (deep sleep), `expired` (subscription lapsed — the collar may be on, but no data arrives either way), `unknown` (never polled; never rendered as a claim about the device)
- Expiry is decided by the `active` flag alone: `remaining_days` legitimately hits zero on a renewal day, and calling a renewing subscription expired would blank the indicator for a working collar. `remaining_days` is still served so the UI can warn before a subscription silences a tracker
- `lastReportAt()` reads `date_tracker`, deliberately NOT weenect-go's `GetTimestamp()` order (`last_message`, `date_server`, `date_tracker`). While a collar is quiet Weenect mutates its last position record in place — same id, `type` flipped to `CMD-X`, `date_server` advanced to now, `battery` raised to the live reading — so `date_server` says the collar reported seconds ago when it has produced nothing for hours. `date_tracker` is stamped by the tracker and stops when the tracker does. (The tracker's clock freezes while off, so just after a switch-on one record can read hours old; the next real fix corrects it.)
- `TrackerState.Reported()` overlays staleness at response time (not at poll time, since it depends on now): a nominally-live collar that has not reported for `powerStaleAfter` (30 min, chosen against the observed `date_tracker` gap distribution — median 1 min, p95 ~6 min, p99 ~21 min) is reported as `stale`
- **Battery is device state, not fix data.** The same in-place mutation raises the live battery on a record whose `timestamp` stays at the last real fix, so the stored row keeps the charge the collar had when it last fixed — 53% on a collar the Weenect app correctly shows at 100% while it charges. `InsertPosition`'s `ON CONFLICT` deliberately does not refresh `battery`/`timestamp`/`type` (that would falsify the historical series); instead the polled battery lives in `power_battery` and both `/api/trackers` and `/api/status` serve it, with `/api/status` overriding its own `battery` field. The web UI prefers it and dims a stored-fix fallback (`.battery.from-fix`)
- State is persisted in the `trackers` table's `power_*` columns, so it survives restarts and is served without an upstream call. `newPowerStatus()` renders it as the `power` object on `/api/trackers` and `/api/status`, returning nil when never polled

### gate.go
Shared fix-quality gate (`ClassifyFix`): fixes are `FixInvalid` (no valid signal, or CMD-WIFIP WiFi-positioned fixes — sticky coordinates, zero satellites, neither GPS nor home-zone evidence), `FixHome` (snapped WiFi-zone fixes — "at home" evidence only), or `FixGPS` (usable for movement analytics). In-zone fixes are marked by `wifi_zone_id != 0`, not by type (the observed types are CMD-WBT/CMD-WBT-OUT and in-zone ALM-ON; the originally assumed CMD-X never occurs but is still mapped to FixHome as future-proofing). NULL `wifi_zone_id` (pre-migration rows) is "not home", never FixHome. All analytics must pass fixes through this gate; `FixHome` coordinates are snapped to the zone center and fabricate stillness.

### dwell.go
Dwell-point waypoints and territory transition graph (issue #2):
- `detectDwellEpisodes()` groups consecutive gated GPS fixes within 30 m of an anchor into stationary episodes (min 3 min span, max 20 min fix gap)
- `dbscanEpisodes()` clusters episode centroids (epsilon 20 m, minPoints 1, pure Go, O(n²))
- `computeWaypoints()` aggregates clusters (dwell time, visits, hour-of-day histogram, last visit, share + previous-period share) and reconciles them with the `waypoints` table by centroid proximity (40 m match radius) so user names survive recomputation; unnamed waypoints get rank-based auto-labels ("Waypoint 1" = most dwelt). Waypoints whose last visit is older than `waypointPruneDays` (10 days) are pruned: the cluster is dropped from the response and the stored row (name included) is deleted; a revisited place is re-detected as a fresh waypoint
- Transitions between consecutive episodes in different clusters form the directed territory graph

### kde.go
KDE utilization distributions — 50% core range vs 95% home range contours (issue #3):
- Gated `FixGPS` fixes only, weighted by `FixWeight(radius)`, projected to a local equirectangular plane (meters) around the data centroid; deterministic stride subsampling caps fixes at 20k per evaluation
- Weighted Gaussian KDE on a 128×128 grid over the fix bounding box padded by 3 bandwidths; bandwidth = 2× median fix `radius` clamped to 10–50 m; evaluation only visits cells within 3 bandwidths of each fix
- `isoplethThreshold()` finds the density where cumulative mass reaches 50%/95% (cells sorted by density descending); `contourRings()` extracts the isopleths with a compact marching-squares, chains segments into rings, simplifies with Douglas-Peucker, and emits GeoJSON-style `[lon,lat]` rings
- `overlapIndex()` is the Bhattacharyya affinity between two cats' KDEs on a shared grid (`sum(sqrt(m1·m2))` over per-cell probability masses: 1 = identical, 0 = disjoint), computed per month for the overlap chart

### presence.go
WiFi-zone at-home timeline and excursion stats (issue #4):
- `zoneState()` classifies a fix: `wifi_zone_id != 0` = home (on the property), `wifi_zone_id == 0` or `wifi_zone_id_exited` set = away; NULL `wifi_zone_id` (pre-migration rows) is unknown — such fixes are skipped entirely and never fabricate state
- `derivePresenceIntervals()` cuts an interval at every zone-state change; a silence over `presenceMaxGapS` (1h) between zone-known fixes closes the interval so it never spans a signal outage
- `summarizePresenceDays()` clips intervals to calendar days (activity timezone) and summarizes each: `time_home_s`, excursion count (attributed to the departure day), `longest_excursion_s`
- `computeExcursions()` aggregates away intervals: excursion list, fixed-bucket duration histogram (<5m … ≥8h), departure hour-of-day histogram
- `currentPresence()` walks the stream backwards (bounded by `presenceLookbackDays`) to find the ongoing state and its run start for the status chip
- WiFi-zone data is the single source of truth for presence state. The SureHub flap's inside/outside status is known-stale (owners often let the cats out themselves, so many exits have no flap event; a wrongly-"outside" flap state never self-corrects) and is display-only — a flap-derived "garden" state was considered and dropped for this reason

### behavior.go
HMM behavioral segmentation — rest / forage / transit (issue #5):
- `resampleBehaviorSteps()` gates fixes (FixGPS only) into 60s slots (nearest fix within 30s of slot center) and derives steps between consecutive occupied slots; a missed slot or an impossible-speed jump (`activityMaxSpeedMS`) breaks the segment, so no step or turn ever spans a signal gap
- 3-state HMM: gamma emissions on step length, von Mises (mu=0) on turning angle; turns are only used when both adjacent steps clear 5 m (below that heading is GPS jitter), so rest is identified by step length alone
- `fitHMM()` runs Baum-Welch (scaled forward-backward, gamma moment-match and Banerjee kappa M-steps) on the trailing 60 days (max 30k most-recent steps, min 500); EM is seeded from step-length quantiles and `relabelHMM()` enforces the semantic ordering rest < forage < transit by mean step length AFTER fitting — never assume the initial ordering survives
- Fitted params persist as JSON in `behavior_models`; `behaviorModel()` refits on demand when the model is missing or older than 7 days (stale model keeps serving if a refit lacks data; no model at all → endpoints report `model_ready: false`)
- `viterbiHMM()` decodes request windows against the stored params; every step counts 60s of its state for the per-day budget and hour-of-day share matrix

### proximity.go
Inter-cat proximity — together-episodes, patrol shifts, lead/follow (issue #6); always analyzes the first two trackers in roster order (same pair as `/api/range/overlap`):
- The collars don't sample at aligned timestamps, so both gated tracks (FixGPS only, shared equirectangular frame around the joint centroid) are interpolated onto a shared 60s grid; a grid point is filled only when its bracketing fixes are ≤ `proxMaxInterpGapS` (3 min) apart, so neither the distance series nor an episode ever spans a signal gap
- `proximitySeries()` emits the inter-cat distance per grid point where both cats have data and segments close runs (< `proxTogetherDistM` = 20 m, sustained ≥ `proxTogetherMinDurS` = 5 min) into together-episodes carrying both cats' decimated `[lat,lon]` segments for the map
- `computeShiftGrid()` buckets both cats' presence intervals (reused from presence.go) into day × hour cells (neither / a / both / b / nodata); a cat is "out" for an hour when its away coverage beats its home coverage, and hours where either cat lacks zone data are nodata, never fabricated
- Lead/follow statistic: the departure-follow fraction — of the leader's excursions (away intervals) where the follower was verifiably home at departure time, the share where the follower left the home zone within `followWindowS` (10 min); excursions where the follower was already out or unknown are ineligible. Chosen over movement-vector cross-correlation: it needs no aligned movement data and matches the question owners ask

### alerts.go
Trapped-cat / out-of-territory alerting (issue #7) — a transparent heuristic over the other analytics' outputs, no ML:
- Evaluation runs after each successful sync (`SyncWorker.SyncAll` evaluates all trackers, `SyncTracker` the one) and reconciles the `alerts` table: raise when a condition fires and no active alert exists, refresh the locate position while firing, set `cleared_at` when it stops. Active state always comes from the DB, so restarts and repeated evaluations stay idempotent. When the newest fix is older than `trappedStaleFix` (1h) the data is too stale to judge and alert state is left untouched
- "Possibly trapped": over the trailing `trappedWindow` (3.5h), gated FixGPS fixes must span (almost) the whole window, number ≥ `trappedMinFixes` (5), and lie within `trappedStationaryFactor` (2) × their median accuracy radius of the centroid; the latest zone-known fix must not be home; and the spot must not be a known dwell waypoint — dwell episodes are recomputed from history ending at the start of the current stationary run (`stationaryRunStart` walks back with dwell.go's continuity rule), so the ongoing stay can never define its own waypoint and suppress the alert
- "Out of territory": the 95% home-range contour (kde.go `computeRange`) over the trailing 30 days ending `territoryRefLag` (1h) before now — the lag keeps candidate fixes out of their own reference polygon; alert when the last `territoryConsecutiveFixes` (3) gated fixes are all > `territoryMinOutsideM` (100 m) outside it (pure-Go point-in-polygon + distance-to-ring). No contour (too little training data) → no alert
- Phone-push delivery (ntfy/webhook) is deliberately NOT implemented — follow-up per the issue

### scheduler.go
Cron-based scheduling using github.com/robfig/cron/v3. Runs sync jobs on configured schedule with graceful shutdown support.

Alongside the cron it runs the collar state poller (`runStatePoller`, `tracker_state_interval_s`, default 60s, immediate first poll so a fresh daemon is not `unknown` for a whole interval). It is deliberately separate from the sync cron: whether a tracker is switched on is a question about now, and the cron is coarse (nightly by default). A state change fires `onStateChange` — wired to `APIServer.NotifyUpdate` — so browsers learn a collar came off without waiting for the next position sync.

### ratelimit.go
Token bucket rate limiter to respect API limits (default: 4 req/sec).

### logger.go
Structured logging using slog. Debug level logs all API requests/responses and rate limiter activity.

### api.go
HTTP API server providing:
- `GET /api/trackers` - List all trackers with position counts and a `power` object (collar on/off state, `off_reason`, `battery`, `battery_state`, `last_fix_at`, `checked_at`, `subscription_active`, `remaining_days`; absent when never polled — see power.go)
- `GET /api/positions/{id}` - Get positions for a tracker (with date range query params)
- `GET /api/status` - Radar status endpoint returning latest positions, home coords, POIs, and SureHub pet status
- `GET /api/activity` - Per-cat daily activity summary
- `GET /api/waypoints?tracker_id=&days=` - Detected dwell-point waypoints with centroid, dwell time, share (+ previous period), visits, hour-of-day histogram, last visit, and name (stored or auto-label)
- `GET /api/waypoints/transitions?tracker_id=&days=` - Directed territory-graph edges `{from, to, count, probability}`
- `PUT /api/waypoints/{id}` - Assign or clear a waypoint's user name (JSON body `{"name": "..."}`)
- `GET /api/range?tracker_id=&month=YYYY-MM` (or `from`/`to` RFC3339; default rolling 30d) - KDE 50% core and 95% home-range contour rings with `bandwidth_m` and `fixes_used`
- `GET /api/range/overlap?months=12` - Monthly cat-vs-cat Bhattacharyya overlap series `[{month, index, fixes_a, fixes_b}]` (index null when either cat lacks fixes)
- `GET /api/presence?tracker_id=&days=7` - WiFi-zone presence timeline: per-day `{start, end, state}` intervals (home/away) clipped to the day plus `{time_home_s, excursions, longest_excursion_s}` summaries
- `GET /api/presence/excursions?tracker_id=&days=30` - Excursion list with duration histogram (`duration_bucket_s` edges + `duration_counts`) and `departure_hours` histogram
- `GET /api/behavior/segments?tracker_id=&from=&to=` (RFC3339, default last 24h) - Viterbi-decoded 60s steps `{t, from, to, state, step_m}` for state-colored tracks; capped at 10000 steps (most recent kept, `truncated` flag), `model_ready: false` when no HMM could be fitted yet
- `GET /api/behavior/budget?tracker_id=&days=7` - Per-day decoded seconds per state (`rest_s`/`forage_s`/`transit_s`) for stacked-bar budgets
- `GET /api/behavior/matrix?tracker_id=&days=30` - 24×3 hour-of-day × state share matrix
- `GET /api/proximity?date=YYYY-MM-DD` (or `from`/`to` RFC3339, max 48h; default today) - inter-cat distance series `[{t, distance_m}]` on the shared 60s grid plus together-episodes `[{start, end, duration_s, points_a, points_b}]` with both cats' paired track segments
- `GET /api/proximity/shifts?days=7` - patrol-shift grid: per-day 24 hourly cells (`neither`/`a`/`both`/`b`/`nodata`) of which cat was out, from presence intervals
- `GET /api/proximity/leadfollow?days=30` - both directions of the departure-follow statistic `{leader, follower, departures, followed, share}` with `follow_window_s`
- `GET /api/alerts?days=30` - alert event log (issue #7), newest first: `{id, tracker_id, name, kind, started_at, cleared_at, duration_s, outcome (active|self_resolved), lat, lon, radius, details}`
- `POST /api/sync` - Manual upstream resync
- `GET /api/events` - SSE stream notifying browsers when fresh data lands
- `GET /health` - Health check endpoint
- Static file serving from `./web` directory

The `/api/status` endpoint integrates with SureHub (github.com/perbu/go-sure) to fetch pet inside/outside status. Results are cached for 5 minutes to avoid excessive API calls. It also adds a per-tracker `presence: {state, since}` chip (issue #4) derived from WiFi-zone data alone — the flap status stays display-only (see presence.go) — and an `alerts` array of active trapped/out-of-territory alerts (issue #7: `{id, tracker_id, name, kind, since, lat, lon, radius}`) for the status-page banner. Each tracker also carries the same `power` object as `/api/trackers` (power.go), and the top-level `battery` field is overridden with the polled device battery when available, so the radar shows the same charge as the Weenect app. `web/status.html` does not read the `power` object itself yet.

### web/index.html + web/app.js
Leaflet map view: per-cat heatmaps, recent-movement trails, activity cat cards
(each carrying the collar's on/off state from `/api/trackers`' `power` object:
a hollow dot plus an `off` / `no signal` / `no fix` / `asleep` / `no sub` pill,
prose replacing "Last seen" with the cause and how long since the last fix,
the polled device battery in preference to the stored fix's (a fallback is
marked `.battery.from-fix` and dimmed), dimmed activity bars because those are
last-known rather than current,
and an amber warning when a subscription is within a week of silencing a
working collar — see power.go; `refreshTrackers()` re-fetches the roster on
every SSE update so the indicator reacts as soon as a collar switches, and the
cards re-render each minute so the durations stay truthful),
plus the waypoint layer (markers sized by dwell share, per-cat accent colors,
click for stats popup with rename), the toggleable transition-graph overlay
(curved directed edges, width ∝ probability), and a "Top places" sidebar panel
ranking waypoints by dwell share with a trend vs. the previous period. The
"Home range" card adds the KDE contour layer (50% core = stronger fill, 95%
home range = outline, per-cat accent colors and toggles) with a month picker,
and the "Territory overlap" card charts the monthly overlap index — clicking a
bar steps the contour layer to that month. The right-rail cards follow the
global date-range selector (7d/30d/90d/custom): the "At home" card stacks a
24h presence ribbon per day (home/away/no-data segments, clamped to 14 days),
the "Excursions" card shows per-cat duration and departure-hour histograms,
the "Behavior" card (issue #5) toggles state-colored tracks on the map
(blue rest / amber forage / red transit, straight segments so state
boundaries stay exact) and shows the per-cat "Activity budget" (stacked
state-share column per day, clamped to 31 days) and "State by hour" (24
stacked micro-bars of state share per hour of day). The "Together"
card (issue #6) charts the inter-cat distance for a selectable day (log
scale, together episodes shaded, dashed threshold rule) and toggles the
together-episode overlay (paired segments in both cats' accent colors); the
"Patrol shifts" card shows the day × hour grid of who was out (per-cat
accents, ink for both, gray for both home, hollow for no data, clamped to
14 days); the "Lead / follow" card states both directions of the
departure-follow share. Three cards keep their own time pickers: the
month-based "Home range" and "Territory overlap" cards, and the single-day
"Together" chart.

### web/status.html
Radar-style status page showing:
- Trackers positioned relative to home on a 1km radius radar
- Distance and bearing to each tracker
- Battery percentage
- WiFi-zone presence chip per cat ("home" / "out for 2h 14m")
- Inside/outside status from SureHub pet flaps
- Configurable POIs as markers
- Alert banner (issue #7): one line per active alert (cat, condition,
  duration) with a "locate" button that recenters the radar on the alert's
  last fix at a tighter range with an accuracy circle (`radius`), plus a
  "reset view" button; the banner clears automatically when the alert does
- Alert history panel: the last 30 days of alerts from `/api/alerts` with
  outcome (ongoing / self-resolved) and duration, for threshold tuning
- Auto-refreshes every 30 seconds

## Key Design Patterns

**Idempotency**: Position records use API-provided IDs as primary keys. Inserts are `ON CONFLICT(id) DO UPDATE` on the extended columns only, so re-syncing never duplicates and re-backfills refresh quality fields.

**Resumability**: Each tracker stores `last_sync_timestamp`. Syncs are incremental from this timestamp. Updated after each successful 24h chunk, so interrupted syncs can resume.

**24-hour chunking**: API has 24h max range. `fetchAndStorePositions()` automatically splits larger ranges and updates sync time after each chunk.

**Rate limiting**: All API calls go through `rateLimiter.Wait()` before execution.

**Device state vs. position data**: Anything about the tracker itself — is it on, how charged is it, is the subscription alive — comes from Weenect's tracker endpoint and is stored on the `trackers` row, never read off the position stream (power.go explains why). Positions describe the cat at a point in time; the `power_*` columns describe the collar now. Weenect mutates a tracker's newest position record in place while the collar is quiet, so any *current* device fact read from that row is silently stale.

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
