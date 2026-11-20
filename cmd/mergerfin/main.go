package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tsrz/mergerfin/internal/admin"
	"github.com/tsrz/mergerfin/internal/api"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/config"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/upstream"
)

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

	if cfg.AdminPass == "admin" {
		logger.Warn("using default admin password; set MERGERFIN_ADMIN_PASS to secure your instance")
	}

	if _, err := auth.EnsureAdminUser(ctx, database, cfg.AdminUser, cfg.AdminPass); err != nil {
		logger.Error("failed to ensure admin user", "username", cfg.AdminUser, "error", err)
		os.Exit(1)
	}

	pool, err := upstream.NewPool(database, cfg.SecretKey, logger)
	if err != nil {
		logger.Error("failed to initialize upstream pool", "error", err)
		os.Exit(1)
	}

	wsHub := api.NewWSHub(database, logger)
	adminHandler := admin.NewHandler(database, pool, nil, logger)

	handler := api.NewRouter(api.RouterDeps{
		Config: cfg,
		DB:     database,
		Pool:   pool,
		Admin:  adminHandler,
		WSHub:  wsHub,
		Syncer: nil,
		Logger: logger,
	})

	serverID, _ := database.GetSetting(ctx, "server_id")
	serverName, _ := database.GetSetting(ctx, "server_name")
	if strings.TrimSpace(serverName) == "" {
		serverName = cfg.ServerName
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
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
			wsHub.CloseAll()
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			wsHub.CloseAll()
			os.Exit(1)
		}
		wsHub.CloseAll()
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
