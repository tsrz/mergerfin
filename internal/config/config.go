package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime configuration loaded from environment variables and CLI flags.
type Config struct {
	Port         string
	DataDir      string
	WebDir       string
	ServerName   string
	AdminUser    string
	AdminPass    string
	SecretKey    string
	SyncInterval time.Duration
	LogLevel     string
}

// Load reads configuration from environment variables and os.Args[1:].
func Load() (*Config, error) {
	return LoadFromArgs(os.Args[1:])
}

// LoadFromArgs reads configuration from environment variables and the supplied CLI arguments.
func LoadFromArgs(args []string) (*Config, error) {
	syncInterval, err := parseSyncInterval(os.Getenv("MERGERFIN_SYNC_INTERVAL"), 30*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("invalid MERGERFIN_SYNC_INTERVAL: %w", err)
	}

	cfg := &Config{
		Port:         envOrDefault("MERGERFIN_PORT", "8096"),
		DataDir:      envOrDefault("MERGERFIN_DATA_DIR", "./data"),
		WebDir:       envOrDefault("MERGERFIN_WEB_DIR", "/app/web"),
		ServerName:   envOrDefault("MERGERFIN_SERVER_NAME", "MergerFin"),
		AdminUser:    envOrDefault("MERGERFIN_ADMIN_USER", "admin"),
		AdminPass:    envOrDefault("MERGERFIN_ADMIN_PASS", "admin"),
		SecretKey:    os.Getenv("MERGERFIN_SECRET_KEY"),
		SyncInterval: syncInterval,
		LogLevel:     envOrDefault("MERGERFIN_LOG_LEVEL", "info"),
	}

	fs := flag.NewFlagSet("mergerfin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.Port, "port", cfg.Port, "HTTP listen port")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data directory path")
	fs.StringVar(&cfg.WebDir, "web-dir", cfg.WebDir, "Web static assets directory path")

	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("parse flags: %w", err)
	}

	return cfg, nil
}

// DBPath returns the SQLite database file path inside DataDir.
func (c *Config) DBPath() string {
	return filepath.Join(c.DataDir, "mergerfin.db")
}

// ImageCacheDir returns the directory path used for cached upstream artwork.
func (c *Config) ImageCacheDir() string {
	return filepath.Join(c.DataDir, "cache", "images")
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseSyncInterval(raw string, def time.Duration) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return def, nil
	}
	if mins, err := strconv.Atoi(trimmed); err == nil {
		if mins <= 0 {
			return 0, fmt.Errorf("must be positive, got %d minutes", mins)
		}
		return time.Duration(mins) * time.Minute, nil
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", d)
	}
	return d, nil
}
