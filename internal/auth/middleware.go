package auth

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/tsrz/mergerfin/internal/db"
)

type contextKey struct{}

// Context holds the authenticated user, session, and parsed client metadata for a request.
type Context struct {
	User    db.ProxyUser
	Session db.ProxySession
	Client  ClientInfo
}

// FromContext retrieves the authenticated *Context from ctx, if present.
func FromContext(ctx context.Context) (*Context, bool) {
	if ctx == nil {
		return nil, false
	}
	ac, ok := ctx.Value(contextKey{}).(*Context)
	if !ok || ac == nil {
		return nil, false
	}
	return ac, true
}

// WithContext returns a child context carrying ac.
func WithContext(ctx context.Context, ac *Context) context.Context {
	return context.WithValue(ctx, contextKey{}, ac)
}

// Authenticate validates the request token against proxy_sessions and proxy_users,
// updates session last_seen_at, and injects *auth.Context into the request context.
func Authenticate(database *db.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ac, ok := resolveAuthContext(r, database)
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "Unauthorized")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithContext(r.Context(), ac)))
		})
	}
}

// RequireAdmin validates authentication like Authenticate and additionally enforces IsAdmin == true.
func RequireAdmin(database *db.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ac, ok := resolveAuthContext(r, database)
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "Unauthorized")
				return
			}
			if !ac.User.IsAdmin {
				writeJSONError(w, http.StatusForbidden, "Forbidden")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithContext(r.Context(), ac)))
		})
	}
}

// OptionalUser injects *auth.Context if a valid token is present, otherwise passes through unchanged.
func OptionalUser(database *db.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ac, ok := resolveAuthContext(r, database); ok {
				r = r.WithContext(WithContext(r.Context(), ac))
			}
			next.ServeHTTP(w, r)
		})
	}
}

func resolveAuthContext(r *http.Request, database *db.DB) (*Context, bool) {
	if database == nil {
		return nil, false
	}
	client := ParseRequest(r)
	if client.Token == "" {
		return nil, false
	}

	ctx := r.Context()
	session, err := database.GetSessionByToken(ctx, client.Token)
	if err != nil {
		return nil, false
	}

	user, err := database.GetUserByID(ctx, session.UserID)
	if err != nil {
		return nil, false
	}

	_ = database.TouchSession(ctx, session.Token)

	return &Context{
		User:    *user,
		Session: *session,
		Client:  client,
	}, true
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
