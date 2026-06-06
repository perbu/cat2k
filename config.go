package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config holds daemon configuration
type Config struct {
	// Weenect API credentials
	Username string `json:"username"`
	Password string `json:"password"`

	// Database configuration
	DatabasePath string `json:"database_path"`

	// Rate limiting (requests per second)
	RateLimit float64 `json:"rate_limit"`

	// Backfill configuration
	BackfillStartDate string `json:"backfill_start_date"` // YYYY-MM-DD format

	// Sync schedule (cron format or "nightly")
	SyncSchedule string `json:"sync_schedule"`

	// Logging
	LogLevel string `json:"log_level"` // debug, info, warn, error

	// HTTP API server
	HTTPListen  string `json:"http_listen"`
	HTTPEnabled bool   `json:"http_enabled"`

	// Home location for radar display (center point)
	HomeLat float64 `json:"home_lat"`
	HomeLon float64 `json:"home_lon"`

	// SureHub credentials (for pet flap status)
	SureHubEmail    string `json:"surehub_email"`
	SureHubPassword string `json:"surehub_password"`

	// Points of interest for radar display
	POIs []POI `json:"pois"`

	// Heatmap configuration
	HeatmapDays int `json:"heatmap_days"` // Number of days to include in heatmap (default: 60)

	// Activity day-bucketing timezone (IANA name, e.g. "Europe/Oslo"). Day
	// boundaries and "today" for the activity panel are computed in this zone.
	// Empty falls back to the server's local timezone.
	ActivityTimezone string `json:"activity_timezone"`
}

// ActivityLocation returns the timezone used to bucket activity into days.
// Falls back to the server's local time if unset or unparseable.
func (c *Config) ActivityLocation() (*time.Location, error) {
	if c.ActivityTimezone == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(c.ActivityTimezone)
	if err != nil {
		return time.Local, fmt.Errorf("invalid activity_timezone %q: %w", c.ActivityTimezone, err)
	}
	return loc, nil
}

// POI represents a point of interest on the radar
type POI struct {
	Name  string  `json:"name"`
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	Color string  `json:"color,omitempty"` // Optional, defaults to gray
}

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		DatabasePath:      "./catboard.db",
		RateLimit:         4.0, // 4 requests per second
		BackfillStartDate: time.Now().AddDate(0, 0, -30).Format("2006-01-02"), // Last 30 days
		SyncSchedule:      "0 2 * * *", // 2am daily (cron format)
		LogLevel:          "info",
		HTTPListen:        ":8080",
		HTTPEnabled:       true,
		HeatmapDays:       60, // Last 60 days for heatmap
	}
}

// getDefaultConfigPaths returns list of default config file paths to check
func getDefaultConfigPaths() []string {
	homeDir, _ := os.UserHomeDir()
	return []string{
		"./config.json",
		"./cat2k.json",
		homeDir + "/.config/cat2k/config.json",
		homeDir + "/.cat2k/config.json",
	}
}

// loadConfig loads configuration from file, env vars, and defaults
func loadConfig(configPath string) (*Config, error) {
	cfg := DefaultConfig()

	// Determine which config file to use
	var configFile string
	if configPath != "" {
		// Explicit config path provided
		configFile = configPath
	} else {
		// Try default locations
		for _, path := range getDefaultConfigPaths() {
			if _, err := os.Stat(path); err == nil {
				configFile = path
				break
			}
		}
	}

	// Load from config file if found
	if configFile != "" {
		data, err := os.ReadFile(configFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file %s: %w", configFile, err)
		}

		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config file %s: %w", configFile, err)
		}
	}

	// Environment variables override config file
	if val := os.Getenv("WEENECT_USERNAME"); val != "" {
		cfg.Username = val
	}
	if val := os.Getenv("WEENECT_PASSWORD"); val != "" {
		cfg.Password = val
	}
	if val := os.Getenv("WEENECT_DATABASE_PATH"); val != "" {
		cfg.DatabasePath = val
	}
	if val := os.Getenv("WEENECT_RATE_LIMIT"); val != "" {
		var rateLimit float64
		if _, err := fmt.Sscanf(val, "%f", &rateLimit); err == nil {
			cfg.RateLimit = rateLimit
		}
	}
	if val := os.Getenv("WEENECT_BACKFILL_START_DATE"); val != "" {
		cfg.BackfillStartDate = val
	}
	if val := os.Getenv("WEENECT_SYNC_SCHEDULE"); val != "" {
		cfg.SyncSchedule = val
	}
	if val := os.Getenv("WEENECT_LOG_LEVEL"); val != "" {
		cfg.LogLevel = val
	}
	if val := os.Getenv("WEENECT_HTTP_LISTEN"); val != "" {
		cfg.HTTPListen = val
	}
	if val := os.Getenv("WEENECT_HTTP_ENABLED"); val != "" {
		cfg.HTTPEnabled = val == "true" || val == "1"
	}
	if val := os.Getenv("WEENECT_HOME_LAT"); val != "" {
		var lat float64
		if _, err := fmt.Sscanf(val, "%f", &lat); err == nil {
			cfg.HomeLat = lat
		}
	}
	if val := os.Getenv("WEENECT_HOME_LON"); val != "" {
		var lon float64
		if _, err := fmt.Sscanf(val, "%f", &lon); err == nil {
			cfg.HomeLon = lon
		}
	}
	if val := os.Getenv("WEENECT_ACTIVITY_TIMEZONE"); val != "" {
		cfg.ActivityTimezone = val
	}
	if val := os.Getenv("SUREHUB_EMAIL"); val != "" {
		cfg.SureHubEmail = val
	}
	if val := os.Getenv("SUREHUB_PASSWORD"); val != "" {
		cfg.SureHubPassword = val
	}

	// Validate required fields
	if cfg.Username == "" {
		return nil, fmt.Errorf("username is required (set WEENECT_USERNAME or use config file)")
	}
	if cfg.Password == "" {
		return nil, fmt.Errorf("password is required (set WEENECT_PASSWORD or use config file)")
	}
	if cfg.ActivityTimezone != "" {
		if _, err := time.LoadLocation(cfg.ActivityTimezone); err != nil {
			return nil, fmt.Errorf("invalid activity_timezone %q (use an IANA name like Europe/Oslo): %w", cfg.ActivityTimezone, err)
		}
	}

	return cfg, nil
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.Username == "" {
		return fmt.Errorf("username is required")
	}
	if c.Password == "" {
		return fmt.Errorf("password is required")
	}
	if c.DatabasePath == "" {
		return fmt.Errorf("database_path is required")
	}
	if c.RateLimit <= 0 {
		return fmt.Errorf("rate_limit must be positive")
	}

	// Validate backfill date format if set
	if c.BackfillStartDate != "" {
		if _, err := time.Parse("2006-01-02", c.BackfillStartDate); err != nil {
			return fmt.Errorf("invalid backfill_start_date format (use YYYY-MM-DD): %w", err)
		}
	}

	return nil
}