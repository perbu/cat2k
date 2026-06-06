package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	gosure "github.com/perbu/go-sure"
)

// APIServer provides HTTP API for tracker data
type APIServer struct {
	db         *Database
	cfg        *Config
	server     *http.Server
	logger     *slog.Logger
	sureClient *gosure.Client

	// Cache for SureHub pet status
	petStatusCache     map[string]petFlapStatus
	petStatusCacheTime time.Time
}

// NewAPIServer creates a new API server
func NewAPIServer(db *Database, cfg *Config, listenAddr string, logger *slog.Logger) *APIServer {
	api := &APIServer{
		db:     db,
		cfg:    cfg,
		logger: logger,
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
	mux.HandleFunc("/health", api.handleHealth)

	// Static file serving for web UI
	fs := http.FileServer(http.Dir("./web"))
	mux.Handle("/", fs)

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
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
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
}

// HistoryPoint represents a position in the tracker's recent history
type HistoryPoint struct {
	Lat       float64   `json:"lat"`
	Lon       float64   `json:"lon"`
	Timestamp time.Time `json:"timestamp"`
}

// TrackerStatus represents a tracker's current status for radar display
type TrackerStatus struct {
	ID        int            `json:"id"`
	Name      string         `json:"name"`
	Color     string         `json:"color"`
	Lat       float64        `json:"lat"`
	Lon       float64        `json:"lon"`
	Battery   *int           `json:"battery,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
	IsInside  *bool          `json:"is_inside,omitempty"`  // From SureHub pet flap
	LastFlap  *string        `json:"last_flap,omitempty"`  // Time of last flap activity
	History   []HistoryPoint `json:"history,omitempty"`    // Recent position history for trail
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
