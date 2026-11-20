package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/tsrz/mergerfin/internal/config"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	syncengine "github.com/tsrz/mergerfin/internal/sync"
	"github.com/tsrz/mergerfin/internal/upstream"
)

type apiHandler struct {
	cfg       *config.Config
	db        *db.DB
	pool      *upstream.Pool
	client    *upstream.Client
	wsHub     *WSHub
	watch     *syncengine.WatchStateManager
	watchOnce sync.Once
	logger    *slog.Logger
}

func newAPIHandler(cfg *config.Config, database *db.DB, pool *upstream.Pool, client *upstream.Client, logger *slog.Logger) *apiHandler {
	if logger == nil {
		logger = slog.Default()
	}
	if client == nil {
		client = upstream.NewClient()
	}
	return &apiHandler{
		cfg:    cfg,
		db:     database,
		pool:   pool,
		client: client,
		logger: logger,
	}
}

func (h *apiHandler) defaultPort() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.Port) != "" {
		return strings.TrimSpace(h.cfg.Port)
	}
	return "8096"
}

func (h *apiHandler) defaultServerName() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.ServerName) != "" {
		return strings.TrimSpace(h.cfg.ServerName)
	}
	return "MergerFin"
}

func (h *apiHandler) serverIdentity(ctx context.Context) (serverID string, serverName string) {
	serverName = h.defaultServerName()
	if h.db == nil {
		return "", serverName
	}
	if id, err := h.db.GetSetting(ctx, "server_id"); err == nil && strings.TrimSpace(id) != "" {
		serverID = strings.TrimSpace(id)
	}
	if name, err := h.db.GetSetting(ctx, "server_name"); err == nil && strings.TrimSpace(name) != "" {
		serverName = strings.TrimSpace(name)
	}
	return serverID, serverName
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func baseURLFromRequest(r *http.Request, defaultPort string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		first := strings.TrimSpace(strings.Split(proto, ",")[0])
		if first != "" {
			scheme = strings.ToLower(first)
		}
	}

	host := strings.TrimSpace(r.Host)
	if host == "" {
		if strings.TrimSpace(defaultPort) == "" {
			defaultPort = "8096"
		}
		host = "localhost:" + strings.TrimSpace(defaultPort)
	}

	return scheme + "://" + host
}

func normalizeRouteID(raw string) (string, bool) {
	id, err := idgen.NormalizeID(raw)
	if err != nil {
		return "", false
	}
	return id, true
}
