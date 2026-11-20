package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/admin/ui"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

const (
	adminCookieName   = "mergerfin_admin_token"
	adminCookieMaxAge = 86400 * 30
)

// SyncStatus describes the current state of the background catalog sync engine.
type SyncStatus struct {
	Running      bool       `json:"running"`
	LastSyncAt   *time.Time `json:"lastSyncAt,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	ItemsIndexed int        `json:"itemsIndexed"`
}

// SyncTrigger allows the admin API to trigger catalog syncs and inspect sync status.
type SyncTrigger interface {
	TriggerSync(ctx context.Context) error
	Status() SyncStatus
}

// Handler serves the embedded /admin UI and /admin/api/* management endpoints.
type Handler struct {
	db     *db.DB
	pool   *upstream.Pool
	syncer SyncTrigger
	logger *slog.Logger
}

// NewHandler constructs an admin Handler.
func NewHandler(database *db.DB, pool *upstream.Pool, syncer SyncTrigger, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		db:     database,
		pool:   pool,
		syncer: syncer,
		logger: logger,
	}
}

// Mount registers all /admin UI and /admin/api/* routes onto r.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	r.Post("/admin/api/login", h.handleLogin)

	r.Group(func(pr chi.Router) {
		pr.Use(auth.RequireAdmin(h.db))

		pr.Post("/admin/api/logout", h.handleLogout)
		pr.Get("/admin/api/me", h.handleMe)
		pr.Get("/admin/api/status", h.handleStatus)

		pr.Get("/admin/api/users", h.handleListUsers)
		pr.Post("/admin/api/users", h.handleCreateUser)
		pr.Delete("/admin/api/users/{id}", h.handleDeleteUser)

		pr.Get("/admin/api/servers", h.handleListServers)
		pr.Post("/admin/api/servers", h.handleCreateServer)
		pr.Put("/admin/api/servers/{id}", h.handleUpdateServer)
		pr.Delete("/admin/api/servers/{id}", h.handleDeleteServer)
		pr.Post("/admin/api/servers/test", h.handleTestServer)

		pr.Post("/admin/api/sync", h.handleSync)
	})

	r.Get("/admin/*", h.handleUI)
}

func (h *Handler) handleUI(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/admin/")
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")

	if rel == "" || rel == "." || rel == "index.html" {
		h.serveIndexHTML(w)
		return
	}

	if strings.HasPrefix(rel, "api/") || rel == "api" {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}

	info, err := fs.Stat(ui.FS, rel)
	if err == nil && !info.IsDir() {
		http.StripPrefix("/admin/", http.FileServer(http.FS(ui.FS))).ServeHTTP(w, r)
		return
	}

	h.serveIndexHTML(w)
}

func (h *Handler) serveIndexHTML(w http.ResponseWriter) {
	data, err := fs.ReadFile(ui.FS, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Admin UI unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// --- Auth ---

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type adminUserDTO struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	IsAdmin   bool      `json:"isAdmin"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "Username and password are required")
		return
	}

	user, err := h.db.GetUserByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "Invalid username or password")
			return
		}
		h.logger.Error("admin login lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	if !auth.VerifyPassword(user.PasswordHash, req.Password) {
		writeError(w, http.StatusUnauthorized, "Invalid username or password")
		return
	}
	if !user.IsAdmin {
		writeError(w, http.StatusForbidden, "Administrator privileges required")
		return
	}

	token := idgen.NewRandomHex32()
	err = h.db.UpsertSession(r.Context(), db.ProxySession{
		Token:      token,
		UserID:     user.ID,
		DeviceID:   idgen.NewRandomHex32(),
		ClientName: "MergerFin Admin",
		DeviceName: "Web Browser",
		AppVersion: "1.0.0",
	})
	if err != nil {
		h.logger.Error("create admin session failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   adminCookieMaxAge,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user": map[string]any{
			"id":       user.ID,
			"username": user.Username,
			"isAdmin":  true,
		},
	})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ac, ok := auth.FromContext(r.Context()); ok && ac.Session.Token != "" {
		if err := h.db.DeleteSession(r.Context(), ac.Session.Token); err != nil {
			h.logger.Warn("delete admin session failed", "err", err)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	ac, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, adminUserDTO{
		ID:        ac.User.ID,
		Username:  ac.User.Username,
		IsAdmin:   ac.User.IsAdmin,
		CreatedAt: ac.User.CreatedAt,
	})
}

// --- Status & Sync ---

type statusCounts struct {
	Movies        int `json:"movies"`
	Series        int `json:"series"`
	Episodes      int `json:"episodes"`
	Sources       int `json:"sources"`
	Users         int `json:"users"`
	ServersTotal  int `json:"servers_total"`
	ServersOnline int `json:"servers_online"`
}

type statusResponse struct {
	ServerID   string       `json:"server_id"`
	ServerName string       `json:"server_name"`
	Counts     statusCounts `json:"counts"`
	SyncStatus SyncStatus   `json:"syncStatus"`
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	serverID, _ := h.db.GetSetting(ctx, "server_id")
	serverName, _ := h.db.GetSetting(ctx, "server_name")

	var counts statusCounts
	rows, err := h.db.SqlDB().QueryContext(ctx, `SELECT item_type, COUNT(*) FROM virtual_items GROUP BY item_type`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var itemType string
			var cnt int
			if err := rows.Scan(&itemType, &cnt); err == nil {
				switch itemType {
				case "Movie":
					counts.Movies = cnt
				case "Series":
					counts.Series = cnt
				case "Episode":
					counts.Episodes = cnt
				}
			}
		}
	}

	_ = h.db.SqlDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM item_sources`).Scan(&counts.Sources)
	if uCount, err := h.db.CountUsers(ctx); err == nil {
		counts.Users = uCount
	}

	if servers, err := h.pool.ListServers(ctx); err == nil {
		counts.ServersTotal = len(servers)
		for _, s := range servers {
			if s.IsOnline {
				counts.ServersOnline++
			}
		}
	}

	var syncStatus SyncStatus
	if h.syncer != nil {
		syncStatus = h.syncer.Status()
	}

	writeJSON(w, http.StatusOK, statusResponse{
		ServerID:   serverID,
		ServerName: serverName,
		Counts:     counts,
		SyncStatus: syncStatus,
	})
}

func (h *Handler) handleSync(w http.ResponseWriter, r *http.Request) {
	if h.syncer == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"started": false,
			"message": "sync engine initializing",
		})
		return
	}

	st := h.syncer.Status()
	if st.Running {
		writeJSON(w, http.StatusOK, map[string]any{
			"started": false,
			"running": true,
			"message": "sync already running",
			"status":  st,
		})
		return
	}

	go func() {
		if err := h.syncer.TriggerSync(context.Background()); err != nil {
			h.logger.Error("triggered catalog sync failed", "err", err)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"started": true,
		"message": "sync started",
		"status":  h.syncer.Status(),
	})
}

// --- Users CRUD ---

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"isAdmin"`
}

func (h *Handler) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		h.logger.Error("list users failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to list users")
		return
	}

	out := make([]adminUserDTO, 0, len(users))
	for _, u := range users {
		out = append(out, adminUserDTO{
			ID:        u.ID,
			Username:  u.Username,
			IsAdmin:   u.IsAdmin,
			CreatedAt: u.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}

	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "Username and password are required")
		return
	}

	if existing, err := h.db.GetUserByUsername(r.Context(), username); err == nil && existing != nil {
		writeError(w, http.StatusConflict, "Username already exists")
		return
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "Failed to check existing user")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to hash password")
		return
	}

	userID := idgen.DeterministicHex32("user:" + strings.ToLower(username))
	err = h.db.CreateUser(r.Context(), db.ProxyUser{
		ID:           userID,
		Username:     username,
		PasswordHash: hash,
		IsAdmin:      req.IsAdmin,
	})
	if err != nil {
		h.logger.Error("create user failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to create user")
		return
	}

	created, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusCreated, adminUserDTO{
			ID:       userID,
			Username: username,
			IsAdmin:  req.IsAdmin,
		})
		return
	}
	writeJSON(w, http.StatusCreated, adminUserDTO{
		ID:        created.ID,
		Username:  created.Username,
		IsAdmin:   created.IsAdmin,
		CreatedAt: created.CreatedAt,
	})
}

func (h *Handler) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	rawID := chi.URLParam(r, "id")
	targetID := rawID
	if norm, err := idgen.NormalizeID(rawID); err == nil {
		targetID = norm
	}

	if ac, ok := auth.FromContext(r.Context()); ok && ac.User.ID == targetID {
		writeError(w, http.StatusBadRequest, "Cannot delete your own active admin account")
		return
	}

	if _, err := h.db.GetUserByID(r.Context(), targetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "User not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to lookup user")
		return
	}

	if err := h.db.DeleteUser(r.Context(), targetID); err != nil {
		h.logger.Error("delete user failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to delete user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// --- Upstream Servers CRUD ---

// serverDTO intentionally omits AccessToken so encrypted or plaintext upstream tokens
// are never exposed over the admin HTTP API.
type serverDTO struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	BaseURL       string     `json:"baseUrl"`
	Username      string     `json:"username"`
	RemoteUserID  string     `json:"remoteUserId,omitempty"`
	Priority      int        `json:"priority"`
	StreamMode    string     `json:"streamMode"`
	Enabled       bool       `json:"enabled"`
	ServerVersion string     `json:"serverVersion,omitempty"`
	IsOnline      bool       `json:"isOnline"`
	LatencyMs     int64      `json:"latencyMs"`
	LastSyncAt    *time.Time `json:"lastSyncAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
}

func toServerDTO(s db.UpstreamServer) serverDTO {
	return serverDTO{
		ID:            s.ID,
		Name:          s.Name,
		BaseURL:       s.BaseURL,
		Username:      s.Username,
		RemoteUserID:  s.RemoteUserID,
		Priority:      s.Priority,
		StreamMode:    s.StreamMode,
		Enabled:       s.Enabled,
		ServerVersion: s.ServerVersion,
		IsOnline:      s.IsOnline,
		LatencyMs:     s.LatencyMs,
		LastSyncAt:    s.LastSyncAt,
		LastError:     s.LastError,
	}
}

type serverUpsertRequest struct {
	Name       string `json:"name"`
	BaseURL    string `json:"baseUrl"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Priority   int    `json:"priority"`
	StreamMode string `json:"streamMode"`
	Enabled    *bool  `json:"enabled"`
}

type serverTestRequest struct {
	BaseURL  string `json:"baseUrl"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := h.pool.ListServers(r.Context())
	if err != nil {
		h.logger.Error("list servers failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to list servers")
		return
	}
	out := make([]serverDTO, 0, len(servers))
	for _, s := range servers {
		out = append(out, toServerDTO(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var req serverUpsertRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	srv, err := h.pool.AddOrUpdateServer(
		r.Context(),
		"",
		req.Name,
		req.BaseURL,
		req.Username,
		req.Password,
		req.Priority,
		req.StreamMode,
		enabled,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toServerDTO(*srv))
}

func (h *Handler) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req serverUpsertRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}

	existing, err := h.db.GetServerByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to lookup server")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = existing.Name
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		baseURL = existing.BaseURL
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		username = existing.Username
	}
	priority := req.Priority
	if priority <= 0 {
		priority = existing.Priority
	}
	streamMode := req.StreamMode
	if streamMode == "" {
		streamMode = existing.StreamMode
	}
	enabled := existing.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	srv, err := h.pool.AddOrUpdateServer(
		r.Context(),
		id,
		name,
		baseURL,
		username,
		req.Password,
		priority,
		streamMode,
		enabled,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Server not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toServerDTO(*srv))
}

func (h *Handler) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.db.GetServerByID(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to lookup server")
		return
	}

	if err := h.pool.DeleteServer(r.Context(), id); err != nil {
		h.logger.Error("delete server failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to delete server")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (h *Handler) handleTestServer(w http.ResponseWriter, r *http.Request) {
	var req serverTestRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}

	res, _ := h.pool.TestConnection(r.Context(), req.BaseURL, req.Username, req.Password)
	writeJSON(w, http.StatusOK, res)
}

// --- JSON Helpers ---

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
