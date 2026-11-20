package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tsrz/mergerfin/internal/config"
	"github.com/tsrz/mergerfin/internal/db"
)

type publicSystemInfo struct {
	LocalAddress           string `json:"LocalAddress"`
	ServerName             string `json:"ServerName"`
	Version                string `json:"Version"`
	ProductName            string `json:"ProductName"`
	OperatingSystem        string `json:"OperatingSystem"`
	ID                     string `json:"Id"`
	StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		logger.Error("failed to create data directory", "path", cfg.DataDir, "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.ImageCacheDir(), 0o755); err != nil {
		logger.Error("failed to create image cache directory", "path", cfg.ImageCacheDir(), "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DBPath(), cfg.ServerName)
	if err != nil {
		logger.Error("failed to open database", "path", cfg.DBPath(), "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := database.Close(); err != nil {
			logger.Error("failed to close database", "error", err)
		}
	}()

	serverID, err := database.GetSetting(ctx, "server_id")
	if err != nil {
		logger.Error("failed to read server_id setting", "error", err)
		os.Exit(1)
	}
	serverName, err := database.GetSetting(ctx, "server_name")
	if err != nil || strings.TrimSpace(serverName) == "" {
		serverName = cfg.ServerName
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /System/Ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Jellyfin Server"))
	})

	mux.HandleFunc("GET /System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = strings.Split(proto, ",")[0]
		}
		host := r.Host
		if host == "" {
			host = "localhost:" + cfg.Port
		}

		info := publicSystemInfo{
			LocalAddress:           scheme + "://" + host,
			ServerName:             serverName,
			Version:                "12.4.0",
			ProductName:            "Jellyfin Server",
			OperatingSystem:        "Linux",
			ID:                     serverID,
			StartupWizardCompleted: true,
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(info); err != nil {
			logger.Warn("failed to write public system info", "error", err)
		}
	})

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/web/index.html", http.StatusFound)
	})

	if info, err := os.Stat(cfg.WebDir); err == nil && info.IsDir() {
		mux.Handle("GET /web/", http.StripPrefix("/web/", http.FileServer(http.Dir(cfg.WebDir))))
		logger.Info("serving static web assets", "dir", cfg.WebDir)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting MergerFin server", "addr", srv.Addr, "server_id", serverID, "server_name", serverName)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("http server error", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
		if err := <-errCh; err != nil {
			logger.Error("http server error after shutdown", "error", err)
			os.Exit(1)
		}
		logger.Info("server stopped")
	}
}

func newLogger(levelStr string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(strings.TrimSpace(levelStr)) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
