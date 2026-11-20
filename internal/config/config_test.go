package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFromArgsDefaults(t *testing.T) {
	for _, k := range []string{
		"MERGERFIN_PORT",
		"MERGERFIN_DATA_DIR",
		"MERGERFIN_WEB_DIR",
		"MERGERFIN_SERVER_NAME",
		"MERGERFIN_ADMIN_USER",
		"MERGERFIN_ADMIN_PASS",
		"MERGERFIN_SECRET_KEY",
		"MERGERFIN_SYNC_INTERVAL",
		"MERGERFIN_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}

	cfg, err := LoadFromArgs(nil)
	if err != nil {
		t.Fatalf("LoadFromArgs(nil) error = %v", err)
	}

	if cfg.Port != "8096" {
		t.Errorf("Port = %q, want %q", cfg.Port, "8096")
	}
	if cfg.DataDir != "./data" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "./data")
	}
	if cfg.WebDir != "/app/web" {
		t.Errorf("WebDir = %q, want %q", cfg.WebDir, "/app/web")
	}
	if cfg.ServerName != "MergerFin" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "MergerFin")
	}
	if cfg.AdminUser != "admin" {
		t.Errorf("AdminUser = %q, want %q", cfg.AdminUser, "admin")
	}
	if cfg.AdminPass != "admin" {
		t.Errorf("AdminPass = %q, want %q", cfg.AdminPass, "admin")
	}
	if cfg.SecretKey != "" {
		t.Errorf("SecretKey = %q, want empty", cfg.SecretKey)
	}
	if cfg.SyncInterval != 30*time.Minute {
		t.Errorf("SyncInterval = %v, want %v", cfg.SyncInterval, 30*time.Minute)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}

	if got, want := cfg.DBPath(), filepath.Join("./data", "mergerfin.db"); got != want {
		t.Errorf("DBPath() = %q, want %q", got, want)
	}
	if got, want := cfg.ImageCacheDir(), filepath.Join("./data", "cache", "images"); got != want {
		t.Errorf("ImageCacheDir() = %q, want %q", got, want)
	}
}

func TestLoadFromArgsEnvAndFlags(t *testing.T) {
	t.Setenv("MERGERFIN_PORT", "9090")
	t.Setenv("MERGERFIN_DATA_DIR", "/var/lib/mergerfin")
	t.Setenv("MERGERFIN_WEB_DIR", "/srv/web")
	t.Setenv("MERGERFIN_SERVER_NAME", "HomeMedia")
	t.Setenv("MERGERFIN_ADMIN_USER", "rootadmin")
	t.Setenv("MERGERFIN_ADMIN_PASS", "s3cr3t")
	t.Setenv("MERGERFIN_SECRET_KEY", "my-secret-key")
	t.Setenv("MERGERFIN_SYNC_INTERVAL", "5m")
	t.Setenv("MERGERFIN_LOG_LEVEL", "debug")

	// Flags override env vars.
	cfg, err := LoadFromArgs([]string{"-port", "7777", "-data-dir", "/tmp/mf-data"})
	if err != nil {
		t.Fatalf("LoadFromArgs error = %v", err)
	}

	if cfg.Port != "7777" {
		t.Errorf("Port = %q, want %q", cfg.Port, "7777")
	}
	if cfg.DataDir != "/tmp/mf-data" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/tmp/mf-data")
	}
	if cfg.WebDir != "/srv/web" {
		t.Errorf("WebDir = %q, want %q", cfg.WebDir, "/srv/web")
	}
	if cfg.ServerName != "HomeMedia" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "HomeMedia")
	}
	if cfg.AdminUser != "rootadmin" {
		t.Errorf("AdminUser = %q, want %q", cfg.AdminUser, "rootadmin")
	}
	if cfg.AdminPass != "s3cr3t" {
		t.Errorf("AdminPass = %q, want %q", cfg.AdminPass, "s3cr3t")
	}
	if cfg.SecretKey != "my-secret-key" {
		t.Errorf("SecretKey = %q, want %q", cfg.SecretKey, "my-secret-key")
	}
	if cfg.SyncInterval != 5*time.Minute {
		t.Errorf("SyncInterval = %v, want %v", cfg.SyncInterval, 5*time.Minute)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if got, want := cfg.DBPath(), filepath.Join("/tmp/mf-data", "mergerfin.db"); got != want {
		t.Errorf("DBPath() = %q, want %q", got, want)
	}
	if got, want := cfg.ImageCacheDir(), filepath.Join("/tmp/mf-data", "cache", "images"); got != want {
		t.Errorf("ImageCacheDir() = %q, want %q", got, want)
	}
}

func TestSyncIntervalIntegerMinutes(t *testing.T) {
	t.Setenv("MERGERFIN_SYNC_INTERVAL", "15")

	cfg, err := LoadFromArgs(nil)
	if err != nil {
		t.Fatalf("LoadFromArgs error = %v", err)
	}
	if cfg.SyncInterval != 15*time.Minute {
		t.Errorf("SyncInterval = %v, want %v", cfg.SyncInterval, 15*time.Minute)
	}
}

func TestLoadFromArgsErrors(t *testing.T) {
	t.Run("invalid sync interval", func(t *testing.T) {
		t.Setenv("MERGERFIN_SYNC_INTERVAL", "not-a-duration")
		if _, err := LoadFromArgs(nil); err == nil {
			t.Fatal("expected error for invalid MERGERFIN_SYNC_INTERVAL")
		}
	})

	t.Run("non-positive sync interval", func(t *testing.T) {
		t.Setenv("MERGERFIN_SYNC_INTERVAL", "0")
		if _, err := LoadFromArgs(nil); err == nil {
			t.Fatal("expected error for zero MERGERFIN_SYNC_INTERVAL")
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		t.Setenv("MERGERFIN_SYNC_INTERVAL", "")
		if _, err := LoadFromArgs([]string{"-unknown-flag"}); err == nil {
			t.Fatal("expected error for unknown CLI flag")
		}
	})
}
