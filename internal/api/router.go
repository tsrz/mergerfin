package api

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/tsrz/mergerfin/internal/admin"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/config"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/upstream"
)

// RouterDeps holds all dependencies required to build the HTTP router.
type RouterDeps struct {
	Config *config.Config
	DB     *db.DB
	Pool   *upstream.Pool
	Admin  *admin.Handler
	WSHub  *WSHub
	Syncer admin.SyncTrigger
	Logger *slog.Logger
}

// NewRouter constructs the root HTTP handler with prefix stripping, middlewares,
// Admin routes, WebSocket endpoint, static web assets, and Jellyfin 12.4 API routes.
func NewRouter(deps RouterDeps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	h := newAPIHandler(deps.Config, deps.DB, logger)

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(slogAccessLogger(logger))
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS"},
		AllowedHeaders: []string{"*"},
		ExposedHeaders: []string{"Content-Range", "Accept-Ranges", "Content-Length"},
	}))

	if deps.Admin != nil {
		deps.Admin.Mount(r)
	}

	if deps.WSHub != nil {
		r.Get("/socket", deps.WSHub.HandleWebSocket)
	}

	redirectToWeb := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/web/index.html", http.StatusFound)
	}
	r.Get("/", redirectToWeb)
	r.Get("/web", redirectToWeb)

	if deps.Config != nil && strings.TrimSpace(deps.Config.WebDir) != "" {
		if info, err := os.Stat(deps.Config.WebDir); err == nil && info.IsDir() {
			fileServer := http.StripPrefix("/web/", http.FileServer(http.Dir(deps.Config.WebDir)))
			r.Handle("/web/*", fileServer)
			logger.Info("serving static web assets", "dir", deps.Config.WebDir)
		}
	}

	// Public Jellyfin handshake, branding, localization, and auth endpoints.
	r.Get("/System/Ping", h.handlePing)
	r.Post("/System/Ping", h.handlePing)
	r.Get("/System/Info/Public", h.handleSystemInfoPublic)
	r.Get("/System/Endpoint", h.handleSystemEndpoint)
	r.Get("/Branding/Configuration", h.handleBrandingConfiguration)
	r.Get("/Branding/Css", h.handleBrandingCSS)
	r.Get("/Branding/Css.css", h.handleBrandingCSS)
	r.Get("/QuickConnect/Enabled", h.handleQuickConnectEnabled)
	r.Get("/Plugins", h.handleEmptyArray)
	r.Get("/Playback/BitrateTest", h.handleBitrateTest)
	r.Get("/Localization/Options", h.handleEmptyArray)
	r.Get("/Localization/Countries", h.handleEmptyArray)
	r.Get("/Localization/Cultures", h.handleEmptyArray)
	r.Get("/Localization/ParentalRatings", h.handleEmptyArray)
	r.Get("/Users/Public", h.handlePublicUsers)
	r.Post("/Users/AuthenticateByName", h.handleAuthenticateByName)

	// Authenticated Jellyfin endpoints.
	r.Group(func(ar chi.Router) {
		ar.Use(auth.Authenticate(deps.DB))
		ar.Get("/System/Info", h.handleSystemInfo)
		ar.Get("/Users", h.handleUsers)
		ar.Get("/Users/Me", h.handleUserMe)
		ar.Get("/Users/{userId}", h.handleUserByID)
		ar.Get("/DisplayPreferences/{displayPreferencesId}", h.handleGetDisplayPreferences)
		ar.Post("/DisplayPreferences/{displayPreferencesId}", h.handlePostDisplayPreferences)
		ar.Post("/Sessions/Capabilities", h.handleNoContent)
		ar.Post("/Sessions/Capabilities/Full", h.handleNoContent)
		ar.Post("/Sessions/Logout", h.handleLogout)
		ar.Get("/Sessions", h.handleSessions)
	})

	return stripLegacyPrefix(r)
}

// stripLegacyPrefix strips a leading case-insensitive /emby or /mediabrowser
// segment before routing so legacy Jellyfin/Emby clients hit standard routes.
func stripLegacyPrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		stripped := p

		switch {
		case len(p) >= len("/mediabrowser") && strings.EqualFold(p[:len("/mediabrowser")], "/mediabrowser") && (len(p) == len("/mediabrowser") || p[len("/mediabrowser")] == '/'):
			stripped = p[len("/mediabrowser"):]
		case len(p) >= len("/emby") && strings.EqualFold(p[:len("/emby")], "/emby") && (len(p) == len("/emby") || p[len("/emby")] == '/'):
			stripped = p[len("/emby"):]
		}

		if stripped != p {
			if stripped == "" {
				stripped = "/"
			}
			r2 := r.Clone(r.Context())
			u := *r.URL
			u.Path = stripped
			u.RawPath = ""
			r2.URL = &u
			next.ServeHTTP(w, r2)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func slogAccessLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			level := slog.LevelInfo
			if strings.EqualFold(r.URL.Path, "/System/Ping") {
				level = slog.LevelDebug
			}

			logger.Log(r.Context(), level, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration", time.Since(start),
				"remote_ip", r.RemoteAddr,
			)
		})
	}
}
