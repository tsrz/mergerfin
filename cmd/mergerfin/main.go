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
	syncengine "github.com/tsrz/mergerfin/internal/sync"
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

	upClient := upstream.NewClient()
	wsHub := api.NewWSHub(database, logger)
	syncEngine := syncengine.NewEngine(database, pool, upClient, wsHub, logger)
	adminHandler := admin.NewHandler(database, pool, syncEngine, logger)

	handler := api.NewRouter(api.RouterDeps{
		Config: cfg,
		DB:     database,
		Pool:   pool,
		Client: upClient,
		Admin:  adminHandler,
		WSHub:  wsHub,
		Syncer: syncEngine,
		Logger: logger,
	})

	syncDone := make(chan struct{})
	go runPeriodicSync(ctx, cfg.SyncInterval, database, syncEngine, logger, syncDone)

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
			stop()
			<-syncDone
			wsHub.CloseAll()
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			<-syncDone
			wsHub.CloseAll()
			os.Exit(1)
		}
		<-syncDone
		wsHub.CloseAll()
		if err := <-errCh; err != nil {
			logger.Error("http server error after shutdown", "error", err)
			os.Exit(1)
		}
		logger.Info("server stopped")
	}
}

func runPeriodicSync(ctx context.Context, interval time.Duration, database *db.DB, syncEngine *syncengine.Engine, logger *slog.Logger, done chan<- struct{}) {
	defer close(done)
	if interval <= 0 {
		interval = 30 * time.Minute
	}

	// Delayed initial sync on startup if upstream servers are already configured.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
		servers, err := database.ListEnabledServers(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("failed to check enabled servers for initial sync", "error", err)
			}
		} else if len(servers) > 0 {
			if err := syncEngine.TriggerSync(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("initial upstream sync failed", "error", err)
			}
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := syncEngine.TriggerSync(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("periodic upstream sync failed", "error", err)
			}
		}
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
