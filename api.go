package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gosure "github.com/perbu/go-sure"
)

//go:embed all:web
var webFS embed.FS

// APIServer provides HTTP API for tracker data
type APIServer struct {
	db         *Database
	cfg        *Config
	worker     *SyncWorker
	server     *http.Server
	logger     *slog.Logger
	sureClient *gosure.Client

	// Cache for SureHub pet status
	petStatusCache     map[string]petFlapStatus
	petStatusCacheTime time.Time

	// SSE subscribers: each connected browser gets a channel that receives a
	// signal whenever new data is available. Guarded by sseMu.
	sseMu      sync.Mutex
	sseClients map[chan struct{}]struct{}
}

// NewAPIServer creates a new API server
func NewAPIServer(db *Database, cfg *Config, worker *SyncWorker, listenAddr string, logger *slog.Logger) *APIServer {
	api := &APIServer{
		db:         db,
		cfg:        cfg,
		worker:     worker,
		logger:     logger,
		sseClients: make(map[chan struct{}]struct{}),
	}

	// Initialize SureHub client if credentials are configured
	if cfg.SureHubEmail != "" && cfg.SureHubPassword != "" {
		api.sureClient = gosure.NewClient(cfg.SureHubEmail, cfg.SureHubPassword)
		logger.Info("SureHub client initialized")
	}

	mux := http.NewServeMux()

	// API endpoints
	mux.HandleFunc("/api/trackers", api.handleGetTrackers)
	mux.HandleFunc("/api/positions/", api.handleGetPositions)
	mux.HandleFunc("/api/status", api.handleGetStatus)
	mux.HandleFunc("/api/heatmap", api.handleGetHeatmap)
	mux.HandleFunc("/api/activity", api.handleGetActivity)
	mux.HandleFunc("/api/waypoints", api.handleGetWaypoints)
	mux.HandleFunc("/api/waypoints/", api.handleWaypointRoutes)
	mux.HandleFunc("/api/range", api.handleGetRange)
	mux.HandleFunc("/api/range/overlap", api.handleGetRangeOverlap)
	mux.HandleFunc("/api/presence", api.handleGetPresence)
	mux.HandleFunc("/api/presence/excursions", api.handleGetExcursions)
	mux.HandleFunc("/api/behavior/segments", api.handleGetBehaviorSegments)
	mux.HandleFunc("/api/behavior/budget", api.handleGetBehaviorBudget)
	mux.HandleFunc("/api/behavior/matrix", api.handleGetBehaviorMatrix)
	mux.HandleFunc("/api/proximity", api.handleGetProximity)
	mux.HandleFunc("/api/proximity/shifts", api.handleGetShifts)
	mux.HandleFunc("/api/proximity/leadfollow", api.handleGetLeadFollow)
	mux.HandleFunc("/api/alerts", api.handleGetAlerts)
	mux.HandleFunc("/api/events", api.handleEvents)
	mux.HandleFunc("/api/sync", api.handleSync)
	mux.HandleFunc("/health", api.handleHealth)

	// Static file serving for web UI (embedded)
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(fmt.Sprintf("failed to access embedded web assets: %v", err))
	}
	mux.Handle("/", http.FileServer(http.FS(webRoot)))

	// Wrap with middleware
	handler := api.loggingMiddleware(api.corsMiddleware(mux))

	api.server = &http.Server{
		Addr:         listenAddr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return api
}

// Start starts the HTTP server
func (a *APIServer) Start() error {
	a.logger.Info("Starting HTTP API server", "addr", a.server.Addr)

	if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}
	return nil
}

// Shutdown gracefully shuts down the HTTP server
func (a *APIServer) Shutdown(ctx context.Context) error {
	a.logger.Info("Shutting down HTTP API server")
	return a.server.Shutdown(ctx)
}

// corsMiddleware adds CORS headers for local development
func (a *APIServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs HTTP requests
func (a *APIServer) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Call the next handler
		next.ServeHTTP(w, r)

		duration := time.Since(start)
		a.logger.Debug("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"duration_ms", duration.Milliseconds(),
		)
	})
}

// writeJSON writes a JSON response
func (a *APIServer) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		a.logger.Error("Failed to encode JSON response", "error", err)
	}
}

// writeError writes an error response
func (a *APIServer) writeError(w http.ResponseWriter, status int, message string) {
	a.writeJSON(w, status, map[string]string{"error": message})
}

// handleHealth handles GET /health
func (a *APIServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSync handles POST /api/sync - triggers an immediate upstream resync
// with Weenect. Runs synchronously (so the caller learns when it finishes) and
// returns 409 if a sync is already running. On success, connected browsers are
// notified via SSE so every open page refreshes.
func (a *APIServer) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if a.worker == nil {
		a.writeError(w, http.StatusServiceUnavailable, "Sync not available")
		return
	}

	// A sync can outlast the server's 10s WriteTimeout, so clear the write
	// deadline for this request only.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		a.logger.Debug("Failed to clear write deadline for sync", "error", err)
	}

	// Detach from the request context so a browser navigating away mid-sync
	// doesn't abort it — the fetched data should still be stored.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	a.logger.Info("Manual sync requested via API", "remote_addr", r.RemoteAddr)
	err := a.worker.TrySyncAll(ctx)
	if errors.Is(err, ErrSyncInProgress) {
		a.writeError(w, http.StatusConflict, "A sync is already in progress")
		return
	}
	if err != nil {
		a.logger.Error("Manual sync failed", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Sync failed")
		return
	}

	a.NotifyUpdate()
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// subscribe registers a new SSE client and returns its notification channel.
func (a *APIServer) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	a.sseMu.Lock()
	a.sseClients[ch] = struct{}{}
	a.sseMu.Unlock()
	return ch
}

// unsubscribe removes an SSE client and closes its channel. Guarded by the
// same mutex as NotifyUpdate, so a send can never race a close.
func (a *APIServer) unsubscribe(ch chan struct{}) {
	a.sseMu.Lock()
	if _, ok := a.sseClients[ch]; ok {
		delete(a.sseClients, ch)
		close(ch)
	}
	a.sseMu.Unlock()
}

// NotifyUpdate signals every connected SSE client that new data is available.
// Slow clients (buffer already full) are skipped — the next event or heartbeat
// will catch them up. Safe to call from any goroutine (e.g. the scheduler).
func (a *APIServer) NotifyUpdate() {
	a.sseMu.Lock()
	defer a.sseMu.Unlock()
	for ch := range a.sseClients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// handleEvents handles GET /api/events - a Server-Sent Events stream that emits
// an "update" event whenever fresh data is available, so browsers can refresh
// without polling.
func (a *APIServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		a.writeError(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}

	// This is a long-lived connection, so clear the server's 10s WriteTimeout
	// for this request only — other endpoints keep their timeout.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		a.logger.Debug("Failed to clear write deadline for SSE", "error", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch := a.subscribe()
	defer a.unsubscribe(ch)

	// Tell the client to fetch right away on (re)connect.
	fmt.Fprint(w, "event: update\ndata: connected\n\n")
	flusher.Flush()

	// Heartbeat keeps the connection under the 60s IdleTimeout and lets the
	// server notice dropped clients.
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, "event: update\ndata: sync\n\n")
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleGetTrackers handles GET /api/trackers
func (a *APIServer) handleGetTrackers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackers, err := a.db.GetAllTrackers()
	if err != nil {
		a.logger.Error("Failed to get trackers", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve trackers")
		return
	}

	// Return empty array instead of null if no trackers
	if trackers == nil {
		trackers = []TrackerWithCount{}
	}

	// Render the polled collar state, overlaying staleness against now
	// (power.go), so the left rail can distinguish a switched-off tracker
	// from one that is merely between fixes.
	now := time.Now()
	for i := range trackers {
		trackers[i].Power = newPowerStatus(trackers[i].State, now)
	}

	a.writeJSON(w, http.StatusOK, map[string]interface{}{
		"trackers": trackers,
	})
}

// handleGetPositions handles GET /api/positions/{trackerID}
func (a *APIServer) handleGetPositions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Extract tracker ID from path
	path := strings.TrimPrefix(r.URL.Path, "/api/positions/")
	trackerID, err := strconv.Atoi(path)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "Invalid tracker ID")
		return
	}

	// Check if tracker exists
	exists, err := a.db.TrackerExists(trackerID)
	if err != nil {
		a.logger.Error("Failed to check tracker existence", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	if !exists {
		a.writeError(w, http.StatusNotFound, "Tracker not found")
		return
	}

	// Parse query parameters for date range
	query := r.URL.Query()
	var start, end time.Time

	if startStr := query.Get("start"); startStr != "" {
		start, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Invalid start date format (use RFC3339)")
			return
		}
	} else {
		// Default: last 7 days
		start = time.Now().AddDate(0, 0, -7)
	}

	if endStr := query.Get("end"); endStr != "" {
		end, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Invalid end date format (use RFC3339)")
			return
		}
	} else {
		// Default: now
		end = time.Now()
	}

	// Get positions
	positions, err := a.db.GetPositions(trackerID, start, end)
	if err != nil {
		a.logger.Error("Failed to get positions", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	// Return empty array instead of null if no positions
	if positions == nil {
		positions = []SimplePosition{}
	}

	a.writeJSON(w, http.StatusOK, map[string]interface{}{
		"tracker_id": trackerID,
		"start":      start.Format(time.RFC3339),
		"end":        end.Format(time.RFC3339),
		"count":      len(positions),
		"positions":  positions,
	})
}

// StatusResponse represents the /api/status response for the radar display
type StatusResponse struct {
	Home struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"home"`
	Trackers    []TrackerStatus `json:"trackers"`
	POIs        []POI           `json:"pois"`
	HeatmapDays int             `json:"heatmap_days"`
	Alerts      []AlertStatus   `json:"alerts"` // active trapped/out-of-territory alerts (issue #7)
}

// AlertStatus is an active alert for the /api/status banner (issue #7):
// which cat, which condition, since when, and where to locate it.
type AlertStatus struct {
	ID        int64     `json:"id"`
	TrackerID int       `json:"tracker_id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // "trapped" or "out_of_territory"
	Since     time.Time `json:"since"`
	Lat       float64   `json:"lat"`
	Lon       float64   `json:"lon"`
	Radius    *int      `json:"radius,omitempty"` // accuracy radius of the last fix
}

// HistoryPoint represents a position in the tracker's recent history
type HistoryPoint struct {
	Lat       float64   `json:"lat"`
	Lon       float64   `json:"lon"`
	Timestamp time.Time `json:"timestamp"`
}

// TrackerStatus represents a tracker's current status for radar display
type TrackerStatus struct {
	ID        int             `json:"id"`
	Name      string          `json:"name"`
	Color     string          `json:"color"`
	Lat       float64         `json:"lat"`
	Lon       float64         `json:"lon"`
	Battery   *int            `json:"battery,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	IsInside  *bool           `json:"is_inside,omitempty"` // From SureHub pet flap
	LastFlap  *string         `json:"last_flap,omitempty"` // Time of last flap activity
	Presence  *PresenceStatus `json:"presence,omitempty"`  // WiFi-zone state (issue #4)
	Power     *PowerStatus    `json:"power,omitempty"`     // Collar on/off state (power.go)
	History   []HistoryPoint  `json:"history,omitempty"`   // Recent position history for trail
}

// PresenceStatus is the current WiFi-zone presence chip for /api/status
// (issue #4). State comes from the collar's zone reporting alone; the SureHub
// flap state is known-stale (manual let-outs, missed exits) and stays
// display-only in is_inside/last_flap.
type PresenceStatus struct {
	State PresenceState `json:"state"`
	Since time.Time     `json:"since"`
}

// Color palette for auto-assigning tracker colors
// Chosen for good contrast and nice additive blending (coral + violet = pink)
var trackerColors = []string{
	"#ff7b54", // coral orange (Bella)
	"#a855f7", // violet purple (Luna)
	"#22d3ee", // cyan
	"#facc15", // amber
	"#f472b6", // pink
	"#34d399", // emerald
	"#fb923c", // orange
	"#818cf8", // indigo
}

// handleGetStatus handles GET /api/status - returns latest positions for radar
func (a *APIServer) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	positions, err := a.db.GetLatestPositions()
	if err != nil {
		a.logger.Error("Failed to get latest positions", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	// Fetch pet status from SureHub if configured
	petStatus := make(map[string]petFlapStatus)
	if a.sureClient != nil {
		petStatus = a.fetchPetStatus()
	}

	resp := StatusResponse{}
	resp.Home.Lat = a.cfg.HomeLat
	resp.Home.Lon = a.cfg.HomeLon
	resp.HeatmapDays = a.cfg.HeatmapDays
	if resp.HeatmapDays <= 0 {
		resp.HeatmapDays = 60 // fallback default
	}

	// Time window for history trail (3 hours)
	historyStart := time.Now().Add(-3 * time.Hour)

	for i, p := range positions {
		color := trackerColors[i%len(trackerColors)]
		tracker := TrackerStatus{
			ID:        p.TrackerID,
			Name:      p.TrackerName,
			Color:     color,
			Lat:       p.Latitude,
			Lon:       p.Longitude,
			Battery:   p.Battery,
			Timestamp: p.Timestamp,
			Power:     newPowerStatus(p.State, time.Now()),
		}

		// Battery is device state, so the polled reading wins over the one
		// frozen into the newest stored fix — while a collar sits on the
		// charger Weenect leaves that fix's charge behind at, say, 53% while
		// the device itself is back at 100% (power.go).
		if tracker.Power != nil && tracker.Power.Battery != nil {
			tracker.Battery = tracker.Power.Battery
		}

		// Fetch recent position history for trail
		recentPositions, err := a.db.GetRecentPositions(p.TrackerID, historyStart)
		if err != nil {
			a.logger.Error("Failed to get recent positions for trail", "tracker_id", p.TrackerID, "error", err)
		} else if len(recentPositions) > 0 {
			tracker.History = make([]HistoryPoint, len(recentPositions))
			for j, pos := range recentPositions {
				tracker.History[j] = HistoryPoint{
					Lat:       pos.Latitude,
					Lon:       pos.Longitude,
					Timestamp: pos.Timestamp,
				}
			}
		}

		// Match pet status by name (case-insensitive)
		if status, ok := petStatus[strings.ToLower(p.TrackerName)]; ok {
			tracker.IsInside = &status.isInside
			if status.lastFlap != nil {
				formatted := status.lastFlap.Format(time.RFC3339)
				tracker.LastFlap = &formatted
			}
		}

		// Current WiFi-zone presence (issue #4). Zone data is authoritative;
		// the flap status above is shown raw but never feeds this state
		// (see presence.go for why).
		presFixes, err := a.db.GetPositionsForPresence(p.TrackerID, time.Now().AddDate(0, 0, -presenceLookbackDays))
		if err != nil {
			a.logger.Error("Failed to get presence fixes", "tracker_id", p.TrackerID, "error", err)
		} else if state, since, known := currentPresence(presFixes); known {
			tracker.Presence = &PresenceStatus{State: state, Since: since}
		}

		resp.Trackers = append(resp.Trackers, tracker)
	}

	// Return empty array instead of null if no trackers
	if resp.Trackers == nil {
		resp.Trackers = []TrackerStatus{}
	}

	// Add POIs from config
	if a.cfg.POIs != nil {
		resp.POIs = a.cfg.POIs
	} else {
		resp.POIs = []POI{}
	}

	// Active alerts for the banner (issue #7). Raised/cleared by the alert
	// evaluation after each sync; read-only here.
	resp.Alerts = []AlertStatus{}
	activeAlerts, err := a.db.GetActiveAlerts()
	if err != nil {
		a.logger.Error("Failed to get active alerts", "error", err)
	} else {
		for _, al := range activeAlerts {
			resp.Alerts = append(resp.Alerts, AlertStatus{
				ID:        al.ID,
				TrackerID: al.TrackerID,
				Name:      al.Name,
				Kind:      al.Kind,
				Since:     al.StartedAt,
				Lat:       al.LastLat,
				Lon:       al.LastLon,
				Radius:    al.Radius,
			})
		}
	}

	a.writeJSON(w, http.StatusOK, resp)
}

// petFlapStatus holds the status from SureHub pet flap
type petFlapStatus struct {
	isInside bool
	lastFlap *time.Time
}

// fetchPetStatus retrieves pet inside/outside status from SureHub (with caching)
func (a *APIServer) fetchPetStatus() map[string]petFlapStatus {
	// Cache for 5 minutes (matches default sync schedule)
	cacheDuration := 5 * time.Minute

	// Return cached data if still valid
	if a.petStatusCache != nil && time.Since(a.petStatusCacheTime) < cacheDuration {
		a.logger.Debug("Using cached pet status", "age", time.Since(a.petStatusCacheTime).Round(time.Second))
		return a.petStatusCache
	}

	result := make(map[string]petFlapStatus)

	dashboard, err := a.sureClient.GetDashboard()
	if err != nil {
		a.logger.Error("Failed to fetch SureHub dashboard", "error", err)
		// Return stale cache if available
		if a.petStatusCache != nil {
			a.logger.Debug("Returning stale cache due to error")
			return a.petStatusCache
		}
		return result
	}

	for _, pet := range dashboard.Pets {
		name := strings.ToLower(pet.Name)
		status := petFlapStatus{}

		if pet.Position != nil && pet.Position.Where != nil {
			status.isInside = *pet.Position.Where == gosure.PetPositionInside
			if pet.Position.Since != nil {
				status.lastFlap = pet.Position.Since
			}
		}

		result[name] = status
	}

	// Update cache
	a.petStatusCache = result
	a.petStatusCacheTime = time.Now()

	a.logger.Debug("Fetched pet status from SureHub", "pets", len(result))
	return result
}

// ActivityTracker holds per-day activity metrics for one tracker.
type ActivityTracker struct {
	ID   int           `json:"id"`
	Name string        `json:"name"`
	Days []DayActivity `json:"days"` // oldest first
}

// ActivityResponse represents the /api/activity response.
type ActivityResponse struct {
	Days     int               `json:"days"`
	Trackers []ActivityTracker `json:"trackers"`
}

// handleGetActivity handles GET /api/activity - per-cat daily activity summary.
func (a *APIServer) handleGetActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	days := 7
	if daysStr := r.URL.Query().Get("days"); daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 1 && d <= 30 {
			days = d
		}
	}

	// Active-floor in metres: a hop must clear this to count as "active" for the
	// active-fraction metric. Distance ignores it (always counts every hop).
	// Tunable so the threshold can be swept without a rebuild.
	activeFloorM := 20.0
	if floorStr := r.URL.Query().Get("active_floor"); floorStr != "" {
		if f, err := strconv.ParseFloat(floorStr, 64); err == nil && f >= 0 && f <= 1000 {
			activeFloorM = f
		}
	}

	loc, err := a.cfg.ActivityLocation()
	if err != nil {
		// Validated at startup, so this is unexpected; log and fall back.
		a.logger.Warn("Falling back to server timezone for activity", "error", err)
	}

	// Build the list of calendar days, oldest first, ending today.
	today := time.Now().In(loc)
	startOfToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	start := startOfToday.AddDate(0, 0, -(days - 1))

	dates := make([]string, days)
	for i := 0; i < days; i++ {
		dates[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}

	positionsByTracker, err := a.db.GetPositionsForActivity(start)
	if err != nil {
		a.logger.Error("Failed to get positions for activity", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	trackers, err := a.db.GetAllTrackers()
	if err != nil {
		a.logger.Error("Failed to get trackers", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve trackers")
		return
	}

	resp := ActivityResponse{Days: days, Trackers: []ActivityTracker{}}
	for _, t := range trackers {
		resp.Trackers = append(resp.Trackers, ActivityTracker{
			ID:   t.ID,
			Name: t.Name,
			Days: computeActivity(positionsByTracker[t.ID], dates, a.cfg.HomeLat, a.cfg.HomeLon, activeFloorM, loc),
		})
	}

	a.writeJSON(w, http.StatusOK, resp)
}

// WaypointsResponse represents the /api/waypoints response.
type WaypointsResponse struct {
	TrackerID int        `json:"tracker_id"`
	Days      int        `json:"days"`
	Waypoints []Waypoint `json:"waypoints"`
}

// TransitionsResponse represents the /api/waypoints/transitions response.
type TransitionsResponse struct {
	TrackerID int                  `json:"tracker_id"`
	Days      int                  `json:"days"`
	Edges     []WaypointTransition `json:"edges"`
}

// parseWaypointParams extracts and validates the tracker_id and days query
// parameters shared by the waypoint endpoints. Days defaults to 30.
func (a *APIServer) parseWaypointParams(w http.ResponseWriter, r *http.Request) (trackerID, days int, ok bool) {
	trackerID, err := strconv.Atoi(r.URL.Query().Get("tracker_id"))
	if err != nil || trackerID <= 0 {
		a.writeError(w, http.StatusBadRequest, "Invalid or missing tracker_id")
		return 0, 0, false
	}

	exists, err := a.db.TrackerExists(trackerID)
	if err != nil {
		a.logger.Error("Failed to check tracker existence", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Database error")
		return 0, 0, false
	}
	if !exists {
		a.writeError(w, http.StatusNotFound, "Tracker not found")
		return 0, 0, false
	}

	days = 30
	if daysStr := r.URL.Query().Get("days"); daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 1 && d <= 365 {
			days = d
		}
	}

	return trackerID, days, true
}

// activityLocation returns the configured day-bucketing timezone, falling back
// to the server timezone (same rule as /api/activity).
func (a *APIServer) activityLocation() *time.Location {
	loc, err := a.cfg.ActivityLocation()
	if err != nil {
		a.logger.Warn("Falling back to server timezone", "error", err)
	}
	return loc
}

// handleGetWaypoints handles GET /api/waypoints?tracker_id=&days= - detected
// dwell-point waypoints with stats and (stored or auto) names.
func (a *APIServer) handleGetWaypoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackerID, days, ok := a.parseWaypointParams(w, r)
	if !ok {
		return
	}

	waypoints, _, err := a.db.computeWaypoints(trackerID, days, a.activityLocation())
	if err != nil {
		a.logger.Error("Failed to compute waypoints", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to compute waypoints")
		return
	}
	if waypoints == nil {
		waypoints = []Waypoint{}
	}

	a.writeJSON(w, http.StatusOK, WaypointsResponse{
		TrackerID: trackerID,
		Days:      days,
		Waypoints: waypoints,
	})
}

// handleWaypointRoutes dispatches /api/waypoints/<sub>: "transitions" for the
// transition graph, or a numeric waypoint ID for renaming.
func (a *APIServer) handleWaypointRoutes(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/api/waypoints/")
	if sub == "transitions" {
		a.handleGetTransitions(w, r)
		return
	}

	id, err := strconv.ParseInt(sub, 10, 64)
	if err != nil || id <= 0 {
		a.writeError(w, http.StatusBadRequest, "Invalid waypoint ID")
		return
	}
	a.handleRenameWaypoint(w, r, id)
}

// handleGetTransitions handles GET /api/waypoints/transitions?tracker_id=&days=
// - directed edges of the territory transition graph.
func (a *APIServer) handleGetTransitions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackerID, days, ok := a.parseWaypointParams(w, r)
	if !ok {
		return
	}

	_, transitions, err := a.db.computeWaypoints(trackerID, days, a.activityLocation())
	if err != nil {
		a.logger.Error("Failed to compute transitions", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to compute transitions")
		return
	}
	if transitions == nil {
		transitions = []WaypointTransition{}
	}

	a.writeJSON(w, http.StatusOK, TransitionsResponse{
		TrackerID: trackerID,
		Days:      days,
		Edges:     transitions,
	})
}

// handleRenameWaypoint handles PUT /api/waypoints/{id} with a JSON body
// {"name": "..."} - assigns (or clears, with an empty name) a waypoint's
// user-assigned name.
func (a *APIServer) handleRenameWaypoint(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if !waypointNameValid(name) {
		a.writeError(w, http.StatusBadRequest, "Name too long (max 100 characters)")
		return
	}

	waypoint, err := a.db.GetWaypoint(id)
	if err != nil {
		a.logger.Error("Failed to get waypoint", "waypoint_id", id, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	if waypoint == nil {
		a.writeError(w, http.StatusNotFound, "Waypoint not found")
		return
	}

	var namePtr *string
	if name != "" {
		namePtr = &name
	}
	if err := a.db.SetWaypointName(id, namePtr); err != nil {
		a.logger.Error("Failed to rename waypoint", "waypoint_id", id, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to rename waypoint")
		return
	}

	a.logger.Info("Waypoint renamed", "waypoint_id", id, "tracker_id", waypoint.TrackerID, "name", name)
	a.writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":   id,
		"name": name,
	})
}

// RangeResponse represents the /api/range response: KDE utilization
// distribution contours for one tracker over a period (issue #3).
type RangeResponse struct {
	TrackerID  int            `json:"tracker_id"`
	Month      string         `json:"month"` // YYYY-MM when a calendar month was requested
	From       time.Time      `json:"from"`
	To         time.Time      `json:"to"`
	BandwidthM float64        `json:"bandwidth_m"`
	FixesUsed  int            `json:"fixes_used"`
	Core       [][][2]float64 `json:"core"` // 50% isopleth rings of [lon,lat]
	Home       [][][2]float64 `json:"home"` // 95% isopleth rings of [lon,lat]
}

// OverlapPoint is one month of the cat-vs-cat territory overlap series.
type OverlapPoint struct {
	Month  string   `json:"month"` // YYYY-MM
	Index  *float64 `json:"index"` // Bhattacharyya affinity; null when either cat lacks fixes
	FixesA int      `json:"fixes_a"`
	FixesB int      `json:"fixes_b"`
}

// parseRangeWindow resolves the period for /api/range: an explicit
// month=YYYY-MM (calendar month in the activity timezone) wins; otherwise
// from/to RFC3339; otherwise a rolling 30 days. The end is exclusive.
func (a *APIServer) parseRangeWindow(w http.ResponseWriter, r *http.Request) (from, to time.Time, month string, ok bool) {
	query := r.URL.Query()
	loc := a.activityLocation()

	if monthStr := query.Get("month"); monthStr != "" {
		parsed, err := time.Parse("2006-01", monthStr)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Invalid month format (use YYYY-MM)")
			return from, to, "", false
		}
		from = time.Date(parsed.Year(), parsed.Month(), 1, 0, 0, 0, 0, loc)
		return from, from.AddDate(0, 1, 0), monthStr, true
	}

	if fromStr := query.Get("from"); fromStr != "" {
		var err error
		from, err = time.Parse(time.RFC3339, fromStr)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Invalid from date format (use RFC3339)")
			return from, to, "", false
		}
		if toStr := query.Get("to"); toStr != "" {
			to, err = time.Parse(time.RFC3339, toStr)
			if err != nil {
				a.writeError(w, http.StatusBadRequest, "Invalid to date format (use RFC3339)")
				return from, to, "", false
			}
		} else {
			to = time.Now()
		}
		return from, to, "", true
	}

	to = time.Now()
	return to.AddDate(0, 0, -30), to, "", true
}

// handleGetRange handles GET /api/range?tracker_id=&month=YYYY-MM (or
// from/to) - KDE 50% core-range and 95% home-range contours.
func (a *APIServer) handleGetRange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackerID, err := strconv.Atoi(r.URL.Query().Get("tracker_id"))
	if err != nil || trackerID <= 0 {
		a.writeError(w, http.StatusBadRequest, "Invalid or missing tracker_id")
		return
	}

	exists, err := a.db.TrackerExists(trackerID)
	if err != nil {
		a.logger.Error("Failed to check tracker existence", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	if !exists {
		a.writeError(w, http.StatusNotFound, "Tracker not found")
		return
	}

	from, to, month, ok := a.parseRangeWindow(w, r)
	if !ok {
		return
	}

	est, err := a.db.computeRange(trackerID, from, to)
	if err != nil {
		a.logger.Error("Failed to compute range", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to compute range")
		return
	}

	a.writeJSON(w, http.StatusOK, RangeResponse{
		TrackerID:  trackerID,
		Month:      month,
		From:       from,
		To:         to,
		BandwidthM: est.BandwidthM,
		FixesUsed:  est.FixesUsed,
		Core:       est.Core,
		Home:       est.Home,
	})
}

// handleGetRangeOverlap handles GET /api/range/overlap?months=12 - the
// monthly Bhattacharyya overlap index between the first two trackers' KDEs,
// oldest month first. Months where either cat has too few gated fixes report
// a null index.
func (a *APIServer) handleGetRangeOverlap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	months := 12
	if mStr := r.URL.Query().Get("months"); mStr != "" {
		if m, err := strconv.Atoi(mStr); err == nil && m >= 1 && m <= 36 {
			months = m
		}
	}

	trackers, err := a.db.GetAllTrackers()
	if err != nil {
		a.logger.Error("Failed to get trackers", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve trackers")
		return
	}

	series := []OverlapPoint{}
	if len(trackers) < 2 {
		a.writeJSON(w, http.StatusOK, series)
		return
	}
	idA, idB := trackers[0].ID, trackers[1].ID

	loc := a.activityLocation()
	now := time.Now().In(loc)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -(months - 1), 0)

	// One bounded fetch per cat covering the whole series; bucket by month.
	posA, err := a.db.GetPositionsForRange(idA, monthStart, time.Time{})
	if err != nil {
		a.logger.Error("Failed to get positions for overlap", "tracker_id", idA, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}
	posB, err := a.db.GetPositionsForRange(idB, monthStart, time.Time{})
	if err != nil {
		a.logger.Error("Failed to get positions for overlap", "tracker_id", idB, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	bucket := func(positions []DwellPosition, from, to time.Time) []DwellPosition {
		var out []DwellPosition
		for _, p := range positions {
			if !p.Timestamp.Before(from) && p.Timestamp.Before(to) {
				out = append(out, p)
			}
		}
		return out
	}

	for i := 0; i < months; i++ {
		from := monthStart.AddDate(0, i, 0)
		to := from.AddDate(0, 1, 0)
		mA := bucket(posA, from, to)
		mB := bucket(posB, from, to)

		gatedA, gatedB := 0, 0
		if len(mA) > 0 {
			_, gatedA = centroidProjection(mA)
		}
		if len(mB) > 0 {
			_, gatedB = centroidProjection(mB)
		}

		point := OverlapPoint{
			Month:  from.Format("2006-01"),
			FixesA: gatedA,
			FixesB: gatedB,
		}
		if gatedA >= kdeMinFixes && gatedB >= kdeMinFixes {
			idx := overlapIndex(mA, mB)
			point.Index = &idx
		}
		series = append(series, point)
	}

	a.writeJSON(w, http.StatusOK, series)
}

// PresenceResponse represents the /api/presence response: per-day zone-state
// intervals plus daily summaries (issue #4).
type PresenceResponse struct {
	TrackerID int           `json:"tracker_id"`
	Days      int           `json:"days"`
	Timeline  []PresenceDay `json:"timeline"`
}

// ExcursionsResponse represents the /api/presence/excursions response.
type ExcursionsResponse struct {
	TrackerID int            `json:"tracker_id"`
	Days      int            `json:"days"`
	Stats     ExcursionStats `json:"stats"`
}

// parsePresenceParams extracts and validates the tracker_id and days query
// parameters shared by the presence endpoints. Follows parseWaypointParams,
// but with a caller-supplied default for days.
func (a *APIServer) parsePresenceParams(w http.ResponseWriter, r *http.Request, defaultDays, maxDays int) (trackerID, days int, ok bool) {
	trackerID, err := strconv.Atoi(r.URL.Query().Get("tracker_id"))
	if err != nil || trackerID <= 0 {
		a.writeError(w, http.StatusBadRequest, "Invalid or missing tracker_id")
		return 0, 0, false
	}

	exists, err := a.db.TrackerExists(trackerID)
	if err != nil {
		a.logger.Error("Failed to check tracker existence", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Database error")
		return 0, 0, false
	}
	if !exists {
		a.writeError(w, http.StatusNotFound, "Tracker not found")
		return 0, 0, false
	}

	days = defaultDays
	if daysStr := r.URL.Query().Get("days"); daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 1 && d <= maxDays {
			days = d
		}
	}

	return trackerID, days, true
}

// handleGetPresence handles GET /api/presence?tracker_id=&days=7 - the
// WiFi-zone home/away timeline: per-day intervals (clipped to the day) plus
// daily summaries (time at home, excursion count, longest excursion). Days
// without zone-known fixes come back empty.
func (a *APIServer) handleGetPresence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackerID, days, ok := a.parsePresenceParams(w, r, 7, 90)
	if !ok {
		return
	}

	loc := a.activityLocation()
	today := time.Now().In(loc)
	startOfToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	start := startOfToday.AddDate(0, 0, -(days - 1))

	dates := make([]string, days)
	for i := 0; i < days; i++ {
		dates[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}

	fixes, err := a.db.GetPositionsForPresence(trackerID, start)
	if err != nil {
		a.logger.Error("Failed to get presence fixes", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	a.writeJSON(w, http.StatusOK, PresenceResponse{
		TrackerID: trackerID,
		Days:      days,
		Timeline:  summarizePresenceDays(derivePresenceIntervals(fixes), dates, loc),
	})
}

// handleGetExcursions handles GET /api/presence/excursions?tracker_id=&days=30
// - the excursions (away intervals) of the period with duration and
// departure-hour histograms for the stats panel.
func (a *APIServer) handleGetExcursions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackerID, days, ok := a.parsePresenceParams(w, r, 30, 365)
	if !ok {
		return
	}

	since := time.Now().AddDate(0, 0, -days)
	fixes, err := a.db.GetPositionsForPresence(trackerID, since)
	if err != nil {
		a.logger.Error("Failed to get presence fixes", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	a.writeJSON(w, http.StatusOK, ExcursionsResponse{
		TrackerID: trackerID,
		Days:      days,
		Stats:     computeExcursions(derivePresenceIntervals(fixes), a.activityLocation()),
	})
}

// BehaviorStepJSON is one decoded 60s step for /api/behavior/segments:
// endpoints and state, for state-colored tracks on the map (issue #5).
type BehaviorStepJSON struct {
	T     time.Time  `json:"t"`    // end-of-step timestamp
	From  [2]float64 `json:"from"` // [lat, lon]
	To    [2]float64 `json:"to"`   // [lat, lon]
	State string     `json:"state"`
	StepM float64    `json:"step_m"`
}

// BehaviorSegmentsResponse represents the /api/behavior/segments response.
type BehaviorSegmentsResponse struct {
	TrackerID  int                `json:"tracker_id"`
	From       time.Time          `json:"from"`
	To         time.Time          `json:"to"`
	ModelReady bool               `json:"model_ready"`
	Truncated  bool               `json:"truncated"` // true when only the most recent behaviorMaxSegSteps are returned
	Steps      []BehaviorStepJSON `json:"steps"`
}

// BehaviorBudgetResponse represents the /api/behavior/budget response.
type BehaviorBudgetResponse struct {
	TrackerID  int                 `json:"tracker_id"`
	Days       int                 `json:"days"`
	ModelReady bool                `json:"model_ready"`
	Budget     []BehaviorDayBudget `json:"budget"`
}

// BehaviorMatrixResponse represents the /api/behavior/matrix response: a
// 24-row hour-of-day x state share matrix.
type BehaviorMatrixResponse struct {
	TrackerID  int            `json:"tracker_id"`
	Days       int            `json:"days"`
	ModelReady bool           `json:"model_ready"`
	States     [3]string      `json:"states"`
	Matrix     [24][3]float64 `json:"matrix"` // [hour][state] share of decoded step time
}

// handleGetBehaviorSegments handles GET
// /api/behavior/segments?tracker_id=&from=&to= - time-ordered decoded steps
// with positions and state, for track coloring. from/to are RFC3339 and
// default to the last 24 hours. The payload is capped at behaviorMaxSegSteps
// steps (most recent kept), mirroring the row cap in GetPositions.
func (a *APIServer) handleGetBehaviorSegments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackerID, err := strconv.Atoi(r.URL.Query().Get("tracker_id"))
	if err != nil || trackerID <= 0 {
		a.writeError(w, http.StatusBadRequest, "Invalid or missing tracker_id")
		return
	}
	exists, err := a.db.TrackerExists(trackerID)
	if err != nil {
		a.logger.Error("Failed to check tracker existence", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	if !exists {
		a.writeError(w, http.StatusNotFound, "Tracker not found")
		return
	}

	query := r.URL.Query()
	to := time.Now()
	if toStr := query.Get("to"); toStr != "" {
		to, err = time.Parse(time.RFC3339, toStr)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Invalid to date format (use RFC3339)")
			return
		}
	}
	from := to.Add(-24 * time.Hour)
	if fromStr := query.Get("from"); fromStr != "" {
		from, err = time.Parse(time.RFC3339, fromStr)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Invalid from date format (use RFC3339)")
			return
		}
	}
	if !from.Before(to) {
		a.writeError(w, http.StatusBadRequest, "from must be before to")
		return
	}

	resp := BehaviorSegmentsResponse{
		TrackerID: trackerID,
		From:      from,
		To:        to,
		Steps:     []BehaviorStepJSON{},
	}

	model, err := a.db.behaviorModel(trackerID)
	if err != nil {
		a.logger.Error("Failed to get behavior model", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to load behavior model")
		return
	}
	if model == nil {
		a.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.ModelReady = true

	segs, decoded, err := a.db.decodeBehaviorWindow(trackerID, from, to, model)
	if err != nil {
		a.logger.Error("Failed to decode behavior", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to decode behavior")
		return
	}

	total := countBehaviorSteps(segs)
	if total > behaviorMaxSegSteps {
		resp.Truncated = true
	}
	kept := 0
	for si, seg := range segs {
		for t, s := range seg {
			kept++
			if total-kept >= behaviorMaxSegSteps {
				continue // beyond the cap: keep only the most recent steps
			}
			resp.Steps = append(resp.Steps, BehaviorStepJSON{
				T:     s.t,
				From:  [2]float64{s.fromLat, s.fromLon},
				To:    [2]float64{s.toLat, s.toLon},
				State: behaviorStateNames[decoded[si][t]],
				StepM: math.Round(s.stepM*10) / 10,
			})
		}
	}

	a.writeJSON(w, http.StatusOK, resp)
}

// handleGetBehaviorBudget handles GET /api/behavior/budget?tracker_id=&days=
// - decoded hours per state per calendar day (stacked-bar data). Days default
// to 7 and follow the same calendar-day convention as /api/presence.
func (a *APIServer) handleGetBehaviorBudget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackerID, days, ok := a.parsePresenceParams(w, r, 7, 90)
	if !ok {
		return
	}

	loc := a.activityLocation()
	today := time.Now().In(loc)
	startOfToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	start := startOfToday.AddDate(0, 0, -(days - 1))

	dates := make([]string, days)
	for i := 0; i < days; i++ {
		dates[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}

	resp := BehaviorBudgetResponse{
		TrackerID: trackerID,
		Days:      days,
		Budget:    make([]BehaviorDayBudget, len(dates)),
	}
	for i, d := range dates {
		resp.Budget[i].Date = d
	}

	model, err := a.db.behaviorModel(trackerID)
	if err != nil {
		a.logger.Error("Failed to get behavior model", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to load behavior model")
		return
	}
	if model == nil {
		a.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.ModelReady = true

	segs, decoded, err := a.db.decodeBehaviorWindow(trackerID, start, time.Now(), model)
	if err != nil {
		a.logger.Error("Failed to decode behavior", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to decode behavior")
		return
	}

	resp.Budget = behaviorBudgetPerDay(segs, decoded, dates, loc)
	a.writeJSON(w, http.StatusOK, resp)
}

// handleGetBehaviorMatrix handles GET /api/behavior/matrix?tracker_id=&days=
// - the 24h x 3 state-share matrix (crepuscular-pattern heatmap data). Days
// default to 30 for a stable daily-rhythm estimate.
func (a *APIServer) handleGetBehaviorMatrix(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	trackerID, days, ok := a.parsePresenceParams(w, r, 30, 90)
	if !ok {
		return
	}

	resp := BehaviorMatrixResponse{
		TrackerID: trackerID,
		Days:      days,
		States:    behaviorStateNames,
	}

	model, err := a.db.behaviorModel(trackerID)
	if err != nil {
		a.logger.Error("Failed to get behavior model", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to load behavior model")
		return
	}
	if model == nil {
		a.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.ModelReady = true

	since := time.Now().AddDate(0, 0, -days)
	segs, decoded, err := a.db.decodeBehaviorWindow(trackerID, since, time.Now(), model)
	if err != nil {
		a.logger.Error("Failed to decode behavior", "tracker_id", trackerID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to decode behavior")
		return
	}

	resp.Matrix = behaviorHourMatrix(segs, decoded, a.activityLocation())
	a.writeJSON(w, http.StatusOK, resp)
}

// ProximityResponse represents the /api/proximity response: the inter-cat
// distance series plus together-episodes for one window (issue #6).
type ProximityResponse struct {
	From            time.Time         `json:"from"`
	To              time.Time         `json:"to"`
	GridStepS       float64           `json:"grid_step_s"`
	TogetherDistM   float64           `json:"together_threshold_m"`
	TogetherMinDurS float64           `json:"together_min_duration_s"`
	Series          []DistancePoint   `json:"series"`
	Episodes        []TogetherEpisode `json:"episodes"`
}

// ShiftsResponse represents the /api/proximity/shifts response: the
// day x hour patrol-shift grid (issue #6).
type ShiftsResponse struct {
	Days int        `json:"days"`
	Cats []ShiftCat `json:"cats"` // index 0 is cell state "a", 1 is "b"
	Grid []ShiftDay `json:"grid"`
}

// ShiftCat identifies one cat of the shift grid's pair.
type ShiftCat struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// LeadFollowResponse represents the /api/proximity/leadfollow response.
type LeadFollowResponse struct {
	Days          int          `json:"days"`
	FollowWindowS float64      `json:"follow_window_s"`
	Directions    []FollowStat `json:"directions"`
}

// firstTwoTrackers returns the first two trackers in roster (name) order —
// the same pair /api/range/overlap analyzes. ok=false with fewer than two.
func (a *APIServer) firstTwoTrackers() (catA, catB TrackerWithCount, ok bool, err error) {
	trackers, err := a.db.GetAllTrackers()
	if err != nil {
		return catA, catB, false, err
	}
	if len(trackers) < 2 {
		return catA, catB, false, nil
	}
	return trackers[0], trackers[1], true, nil
}

// parseProximityWindow resolves the window for /api/proximity: an explicit
// date=YYYY-MM-DD (calendar day in the activity timezone) wins; otherwise
// from/to RFC3339 (span capped at proxMaxWindowS); otherwise today.
func (a *APIServer) parseProximityWindow(w http.ResponseWriter, r *http.Request) (from, to time.Time, ok bool) {
	query := r.URL.Query()
	loc := a.activityLocation()

	if dateStr := query.Get("date"); dateStr != "" {
		day, err := time.ParseInLocation("2006-01-02", dateStr, loc)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Invalid date format (use YYYY-MM-DD)")
			return from, to, false
		}
		return day, day.AddDate(0, 0, 1), true
	}

	if fromStr := query.Get("from"); fromStr != "" {
		var err error
		from, err = time.Parse(time.RFC3339, fromStr)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, "Invalid from date format (use RFC3339)")
			return from, to, false
		}
		to = time.Now()
		if toStr := query.Get("to"); toStr != "" {
			to, err = time.Parse(time.RFC3339, toStr)
			if err != nil {
				a.writeError(w, http.StatusBadRequest, "Invalid to date format (use RFC3339)")
				return from, to, false
			}
		}
		if !from.Before(to) {
			a.writeError(w, http.StatusBadRequest, "from must be before to")
			return from, to, false
		}
		if to.Sub(from).Seconds() > proxMaxWindowS {
			a.writeError(w, http.StatusBadRequest, "Window too large (max 48h)")
			return from, to, false
		}
		return from, to, true
	}

	now := time.Now().In(loc)
	from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return from, from.AddDate(0, 0, 1), true
}

// handleGetProximity handles GET /api/proximity?date=YYYY-MM-DD (or from/to,
// default today) - the inter-cat distance series on the shared 60s grid plus
// together-episodes with both cats' paired track segments. Distance points
// only exist where both cats have honestly interpolated grid positions, so
// the series (and episodes) never span a signal gap.
func (a *APIServer) handleGetProximity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	from, to, ok := a.parseProximityWindow(w, r)
	if !ok {
		return
	}

	resp := ProximityResponse{
		From:            from,
		To:              to,
		GridStepS:       proxGridStepS,
		TogetherDistM:   proxTogetherDistM,
		TogetherMinDurS: proxTogetherMinDurS,
		Series:          []DistancePoint{},
		Episodes:        []TogetherEpisode{},
	}

	catA, catB, ok, err := a.firstTwoTrackers()
	if err != nil {
		a.logger.Error("Failed to get trackers", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve trackers")
		return
	}
	if !ok {
		a.writeJSON(w, http.StatusOK, resp)
		return
	}

	posA, err := a.db.GetPositionsForRange(catA.ID, from, to)
	if err != nil {
		a.logger.Error("Failed to get positions for proximity", "tracker_id", catA.ID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}
	posB, err := a.db.GetPositionsForRange(catB.ID, from, to)
	if err != nil {
		a.logger.Error("Failed to get positions for proximity", "tracker_id", catB.ID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	// One shared projection frame around the joint centroid (same approach as
	// the overlap endpoint), so distances come from meter arithmetic.
	proj, _ := centroidProjection(append(posA, posB...))
	gridA := resampleTrack(gateProximityFixes(posA, proj), from, to)
	gridB := resampleTrack(gateProximityFixes(posB, proj), from, to)

	resp.Series, resp.Episodes = proximitySeries(gridA, gridB, proj)
	a.writeJSON(w, http.StatusOK, resp)
}

// handleGetShifts handles GET /api/proximity/shifts?days=7 - the patrol-shift
// grid: per calendar day and hour, which cat was out (neither / a / both /
// b / nodata), derived from the WiFi-zone presence intervals (issue #4 data).
// Days default to 7 and follow the same calendar-day convention as
// /api/presence.
func (a *APIServer) handleGetShifts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	days := 7
	if daysStr := r.URL.Query().Get("days"); daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 1 && d <= 30 {
			days = d
		}
	}

	catA, catB, ok, err := a.firstTwoTrackers()
	if err != nil {
		a.logger.Error("Failed to get trackers", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve trackers")
		return
	}

	resp := ShiftsResponse{Days: days, Grid: []ShiftDay{}}
	if !ok {
		a.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Cats = []ShiftCat{{ID: catA.ID, Name: catA.Name}, {ID: catB.ID, Name: catB.Name}}

	loc := a.activityLocation()
	today := time.Now().In(loc)
	startOfToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	start := startOfToday.AddDate(0, 0, -(days - 1))

	dates := make([]string, days)
	for i := 0; i < days; i++ {
		dates[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}

	fixesA, err := a.db.GetPositionsForPresence(catA.ID, start)
	if err != nil {
		a.logger.Error("Failed to get presence fixes", "tracker_id", catA.ID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}
	fixesB, err := a.db.GetPositionsForPresence(catB.ID, start)
	if err != nil {
		a.logger.Error("Failed to get presence fixes", "tracker_id", catB.ID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	resp.Grid = computeShiftGrid(
		derivePresenceIntervals(fixesA),
		derivePresenceIntervals(fixesB),
		dates, loc,
	)
	a.writeJSON(w, http.StatusOK, resp)
}

// handleGetLeadFollow handles GET /api/proximity/leadfollow?days=30 - the
// lead/follow stat card: both directions of the departure-follow statistic
// (see proximity.go for the exact definition). Days default to 30.
func (a *APIServer) handleGetLeadFollow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	days := 30
	if daysStr := r.URL.Query().Get("days"); daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 1 && d <= 365 {
			days = d
		}
	}

	catA, catB, ok, err := a.firstTwoTrackers()
	if err != nil {
		a.logger.Error("Failed to get trackers", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve trackers")
		return
	}

	resp := LeadFollowResponse{
		Days:          days,
		FollowWindowS: followWindowS,
		Directions:    []FollowStat{},
	}
	if !ok {
		a.writeJSON(w, http.StatusOK, resp)
		return
	}

	since := time.Now().AddDate(0, 0, -days)
	fixesA, err := a.db.GetPositionsForPresence(catA.ID, since)
	if err != nil {
		a.logger.Error("Failed to get presence fixes", "tracker_id", catA.ID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}
	fixesB, err := a.db.GetPositionsForPresence(catB.ID, since)
	if err != nil {
		a.logger.Error("Failed to get presence fixes", "tracker_id", catB.ID, "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}
	intA := derivePresenceIntervals(fixesA)
	intB := derivePresenceIntervals(fixesB)

	depAB, folAB := followStats(intA, intB)
	depBA, folBA := followStats(intB, intA)
	share := func(followed, departures int) float64 {
		if departures == 0 {
			return 0
		}
		return float64(followed) / float64(departures)
	}
	resp.Directions = []FollowStat{
		{LeaderID: catA.ID, Leader: catA.Name, FollowerID: catB.ID, Follower: catB.Name,
			Departures: depAB, Followed: folAB, Share: share(folAB, depAB)},
		{LeaderID: catB.ID, Leader: catB.Name, FollowerID: catA.ID, Follower: catA.Name,
			Departures: depBA, Followed: folBA, Share: share(folBA, depBA)},
	}
	a.writeJSON(w, http.StatusOK, resp)
}

// AlertJSON is one alert event for the /api/alerts history (issue #7).
type AlertJSON struct {
	ID        int64      `json:"id"`
	TrackerID int        `json:"tracker_id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	StartedAt time.Time  `json:"started_at"`
	ClearedAt *time.Time `json:"cleared_at,omitempty"`
	DurationS float64    `json:"duration_s"` // until cleared, or until now while active
	Outcome   string     `json:"outcome"`    // "active" or "self_resolved"
	Lat       float64    `json:"lat"`
	Lon       float64    `json:"lon"`
	Radius    *int       `json:"radius,omitempty"`
	Details   string     `json:"details,omitempty"`
}

// AlertsResponse represents the /api/alerts response.
type AlertsResponse struct {
	Days   int         `json:"days"`
	Alerts []AlertJSON `json:"alerts"` // newest first
}

// handleGetAlerts handles GET /api/alerts?days=30 - the alert event log with
// outcomes, so thresholds can be tuned against false positives. Alerts clear
// only by the condition ending, so a cleared alert is always "self_resolved".
func (a *APIServer) handleGetAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	days := 30
	if daysStr := r.URL.Query().Get("days"); daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 1 && d <= 365 {
			days = d
		}
	}

	since := time.Now().AddDate(0, 0, -days)
	alerts, err := a.db.GetAlertHistory(since)
	if err != nil {
		a.logger.Error("Failed to get alert history", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve alerts")
		return
	}

	now := time.Now()
	out := make([]AlertJSON, 0, len(alerts))
	for _, al := range alerts {
		entry := AlertJSON{
			ID:        al.ID,
			TrackerID: al.TrackerID,
			Name:      al.Name,
			Kind:      al.Kind,
			StartedAt: al.StartedAt,
			ClearedAt: al.ClearedAt,
			Outcome:   "active",
			DurationS: now.Sub(al.StartedAt).Seconds(),
			Lat:       al.LastLat,
			Lon:       al.LastLon,
			Radius:    al.Radius,
			Details:   al.Details,
		}
		if al.ClearedAt != nil {
			entry.Outcome = "self_resolved"
			entry.DurationS = al.ClearedAt.Sub(al.StartedAt).Seconds()
		}
		out = append(out, entry)
	}

	a.writeJSON(w, http.StatusOK, AlertsResponse{Days: days, Alerts: out})
}

// HeatmapBin represents a single bin in the heatmap grid
type HeatmapBin struct {
	X     int `json:"x"`
	Y     int `json:"y"`
	Count int `json:"count"`
}

// HeatmapTrackerData represents heatmap data for a single tracker
type HeatmapTrackerData struct {
	Name  string       `json:"name"`
	Color string       `json:"color"`
	Bins  []HeatmapBin `json:"bins"`
	Max   int          `json:"max"` // Maximum count in any bin (for normalization)
}

// HeatmapResponse represents the /api/heatmap response
type HeatmapResponse struct {
	Resolution int                        `json:"resolution"`
	RadiusM    float64                    `json:"radius_m"`
	Days       int                        `json:"days"`
	Trackers   map[int]HeatmapTrackerData `json:"trackers"`
}

// handleGetHeatmap handles GET /api/heatmap
func (a *APIServer) handleGetHeatmap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Parse query parameters
	query := r.URL.Query()

	days := 30 // default
	if daysStr := query.Get("days"); daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 && d <= 365 {
			days = d
		}
	}

	resolution := 100 // default grid size
	if resStr := query.Get("resolution"); resStr != "" {
		if res, err := strconv.Atoi(resStr); err == nil && res >= 20 && res <= 200 {
			resolution = res
		}
	}

	since := time.Now().AddDate(0, 0, -days)
	radiusM := 1000.0 // radar radius in meters (matches MAX_DISTANCE in JS)

	// Fetch all positions for the time period
	positionsByTracker, err := a.db.GetPositionsForHeatmap(since)
	if err != nil {
		a.logger.Error("Failed to get positions for heatmap", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve positions")
		return
	}

	// Get tracker names
	trackers, err := a.db.GetAllTrackers()
	if err != nil {
		a.logger.Error("Failed to get trackers", "error", err)
		a.writeError(w, http.StatusInternalServerError, "Failed to retrieve trackers")
		return
	}

	// Build tracker info map and determine color assignment
	// Colors must match the /api/status endpoint (assigned by index in sorted order)
	trackerInfo := make(map[int]struct {
		name  string
		color string
	})
	for i, t := range trackers {
		trackerInfo[t.ID] = struct {
			name  string
			color string
		}{
			name:  t.Name,
			color: trackerColors[i%len(trackerColors)],
		}
	}

	homeLat := a.cfg.HomeLat
	homeLon := a.cfg.HomeLon

	resp := HeatmapResponse{
		Resolution: resolution,
		RadiusM:    radiusM,
		Days:       days,
		Trackers:   make(map[int]HeatmapTrackerData),
	}

	for trackerID, positions := range positionsByTracker {
		// Create a 2D grid for binning
		bins := make(map[[2]int]int)
		maxCount := 0

		for _, pos := range positions {
			// Convert lat/lon to radar x,y coordinates (0 to resolution range)
			x, y := a.latLonToRadarBin(pos.Latitude, pos.Longitude, homeLat, homeLon, radiusM, resolution)

			// Skip positions outside the radar
			if x < 0 || x >= resolution || y < 0 || y >= resolution {
				continue
			}

			key := [2]int{x, y}
			bins[key]++
			if bins[key] > maxCount {
				maxCount = bins[key]
			}
		}

		// Convert map to slice
		binSlice := make([]HeatmapBin, 0, len(bins))
		for key, count := range bins {
			binSlice = append(binSlice, HeatmapBin{
				X:     key[0],
				Y:     key[1],
				Count: count,
			})
		}

		info := trackerInfo[trackerID]
		resp.Trackers[trackerID] = HeatmapTrackerData{
			Name:  info.name,
			Color: info.color,
			Bins:  binSlice,
			Max:   maxCount,
		}

		a.logger.Debug("Heatmap data generated",
			"tracker_id", trackerID,
			"positions", len(positions),
			"bins", len(binSlice),
			"max_count", maxCount,
		)
	}

	a.writeJSON(w, http.StatusOK, resp)
}

// latLonToRadarBin converts a lat/lon position to a bin index in the radar grid
func (a *APIServer) latLonToRadarBin(lat, lon, homeLat, homeLon, radiusM float64, resolution int) (int, int) {
	distance := haversineDistance(homeLat, homeLon, lat, lon)
	bearing := calculateBearing(homeLat, homeLon, lat, lon)

	// Normalize distance to 0-1 range (capped at radar edge)
	normalizedDist := distance / radiusM
	if normalizedDist > 1 {
		normalizedDist = 1.1 // Mark as outside
	}

	// Convert polar to cartesian (center at resolution/2)
	// Bearing: 0 = North, 90 = East, etc.
	// In screen coords: 0 degrees = up (negative Y), 90 = right (positive X)
	angleRad := (bearing - 90) * math.Pi / 180
	r := normalizedDist * float64(resolution) / 2

	x := float64(resolution)/2 + r*math.Cos(angleRad)
	y := float64(resolution)/2 + r*math.Sin(angleRad)

	return int(x), int(y)
}

// haversineDistance calculates distance in meters between two lat/lon points
func haversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000 // Earth radius in meters

	phi1 := lat1 * math.Pi / 180
	phi2 := lat2 * math.Pi / 180
	deltaPhi := (lat2 - lat1) * math.Pi / 180
	deltaLambda := (lon2 - lon1) * math.Pi / 180

	a := math.Sin(deltaPhi/2)*math.Sin(deltaPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*
			math.Sin(deltaLambda/2)*math.Sin(deltaLambda/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return R * c
}

// calculateBearing calculates bearing in degrees from point 1 to point 2
func calculateBearing(lat1, lon1, lat2, lon2 float64) float64 {
	phi1 := lat1 * math.Pi / 180
	phi2 := lat2 * math.Pi / 180
	deltaLambda := (lon2 - lon1) * math.Pi / 180

	y := math.Sin(deltaLambda) * math.Cos(phi2)
	x := math.Cos(phi1)*math.Sin(phi2) -
		math.Sin(phi1)*math.Cos(phi2)*math.Cos(deltaLambda)

	bearing := math.Atan2(y, x) * 180 / math.Pi
	return math.Mod(bearing+360, 360)
}
