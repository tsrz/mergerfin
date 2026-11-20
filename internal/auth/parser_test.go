package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
)

func TestParseRequestFormats(t *testing.T) {
	tests := []struct {
		name   string
		target string
		headers map[string]string
		cookie *http.Cookie
		want   ClientInfo
	}{
		{
			name:   "Authorization MediaBrowser with Token",
			target: "/Items",
			headers: map[string]string{
				"Authorization": `MediaBrowser Client="Jellyfin Web", Device="Firefox", DeviceId="dev-123", Version="10.9.0", Token="tok-mb"`,
			},
			want: ClientInfo{
				Client:   "Jellyfin Web",
				Device:   "Firefox",
				DeviceID: "dev-123",
				Version:  "10.9.0",
				Token:    "tok-mb",
			},
		},
		{
			name:   "Authorization MediaBrowser without Token",
			target: "/Users/AuthenticateByName",
			headers: map[string]string{
				"Authorization": `mediabrowser Client="Infuse", Device="Apple TV", DeviceId="atv-1", Version="7.7"`,
			},
			want: ClientInfo{
				Client:   "Infuse",
				Device:   "Apple TV",
				DeviceID: "atv-1",
				Version:  "7.7",
				Token:    "",
			},
		},
		{
			name:   "Authorization Emby with single quotes, unquoted values, and commas inside quotes",
			target: "/Items",
			headers: map[string]string{
				"Authorization": `EMBY Client='My, Client', Device="Chrome, Linux", DeviceId=unquoted-id, Version=12.4.0, Token="tok-emby"`,
			},
			want: ClientInfo{
				Client:   "My, Client",
				Device:   "Chrome, Linux",
				DeviceID: "unquoted-id",
				Version:  "12.4.0",
				Token:    "tok-emby",
			},
		},
		{
			name:   "X-Emby-Authorization header",
			target: "/Items",
			headers: map[string]string{
				"X-Emby-Authorization": `Emby Client="Swiftfin", Device="iPhone", DeviceId="ios-9", Version="1.2.0", Token="tok-xembyauth"`,
			},
			want: ClientInfo{
				Client:   "Swiftfin",
				Device:   "iPhone",
				DeviceID: "ios-9",
				Version:  "1.2.0",
				Token:    "tok-xembyauth",
			},
		},
		{
			name:   "Authorization metadata without Token plus X-Emby-Token",
			target: "/Items",
			headers: map[string]string{
				"Authorization": `MediaBrowser Client="Kodi", Device="LivingRoom", DeviceId="kodi-1", Version="21.0"`,
				"X-Emby-Token":  "tok-xemby",
			},
			want: ClientInfo{
				Client:   "Kodi",
				Device:   "LivingRoom",
				DeviceID: "kodi-1",
				Version:  "21.0",
				Token:    "tok-xemby",
			},
		},
		{
			name:   "X-MediaBrowser-Token",
			target: "/Items",
			headers: map[string]string{
				"X-MediaBrowser-Token": "tok-xmb",
			},
			want: ClientInfo{Token: "tok-xmb"},
		},
		{
			name:   "Authorization Bearer",
			target: "/Items",
			headers: map[string]string{
				"Authorization": "Bearer tok-bearer",
			},
			want: ClientInfo{Token: "tok-bearer"},
		},
		{
			name:   "Query api_key",
			target: "/Videos/123/stream?api_key=tok-q1",
			want:   ClientInfo{Token: "tok-q1"},
		},
		{
			name:   "Query ApiKey",
			target: "/Videos/123/stream?ApiKey=tok-q2",
			want:   ClientInfo{Token: "tok-q2"},
		},
		{
			name:   "Admin cookie",
			target: "/admin/api/status",
			cookie: &http.Cookie{Name: "mergerfin_admin_token", Value: "tok-cookie"},
			want:   ClientInfo{Token: "tok-cookie"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}

			got := ParseRequest(req)
			if got != tc.want {
				t.Fatalf("ParseRequest() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseRequestPrecedence(t *testing.T) {
	// 1. Authorization MediaBrowser beats X-Emby-Authorization, tokens, query, cookie.
	req := httptest.NewRequest(http.MethodGet, "/Items?api_key=q1&ApiKey=q2", nil)
	req.Header.Set("Authorization", `MediaBrowser Client="C1", Token="auth-mb"`)
	req.Header.Set("X-Emby-Authorization", `Emby Client="C2", Token="x-emby-auth"`)
	req.Header.Set("X-Emby-Token", "x-emby-tok")
	req.Header.Set("X-MediaBrowser-Token", "x-mb-tok")
	req.AddCookie(&http.Cookie{Name: "mergerfin_admin_token", Value: "cookie-tok"})
	if got := ParseRequest(req); got.Token != "auth-mb" || got.Client != "C1" {
		t.Fatalf("expected auth-mb & C1, got %+v", got)
	}

	// 2. X-Emby-Authorization beats X-Emby-Token.
	req.Header.Del("Authorization")
	if got := ParseRequest(req); got.Token != "x-emby-auth" || got.Client != "C2" {
		t.Fatalf("expected x-emby-auth & C2, got %+v", got)
	}

	// 3. X-Emby-Token beats X-MediaBrowser-Token and Bearer.
	req.Header.Del("X-Emby-Authorization")
	req.Header.Set("Authorization", "Bearer bearer-tok")
	if got := ParseRequest(req); got.Token != "x-emby-tok" {
		t.Fatalf("expected x-emby-tok, got %+v", got)
	}

	// 4. X-MediaBrowser-Token beats Bearer.
	req.Header.Del("X-Emby-Token")
	if got := ParseRequest(req); got.Token != "x-mb-tok" {
		t.Fatalf("expected x-mb-tok, got %+v", got)
	}

	// 5. Bearer beats api_key.
	req.Header.Del("X-MediaBrowser-Token")
	if got := ParseRequest(req); got.Token != "bearer-tok" {
		t.Fatalf("expected bearer-tok, got %+v", got)
	}

	// 6. api_key beats ApiKey.
	req.Header.Del("Authorization")
	if got := ParseRequest(req); got.Token != "q1" {
		t.Fatalf("expected q1, got %+v", got)
	}

	// 7. ApiKey beats cookie.
	req = httptest.NewRequest(http.MethodGet, "/Items?ApiKey=q2", nil)
	req.AddCookie(&http.Cookie{Name: "mergerfin_admin_token", Value: "cookie-tok"})
	if got := ParseRequest(req); got.Token != "q2" {
		t.Fatalf("expected q2, got %+v", got)
	}

	// 8. Cookie used when nothing else is present.
	req = httptest.NewRequest(http.MethodGet, "/Items", nil)
	req.AddCookie(&http.Cookie{Name: "mergerfin_admin_token", Value: "cookie-tok"})
	if got := ParseRequest(req); got.Token != "cookie-tok" {
		t.Fatalf("expected cookie-tok, got %+v", got)
	}
}

func TestPasswordHashingAndEnsureAdminUser(t *testing.T) {
	hash, err := HashPassword("s3cr3t-pass")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if hash == "s3cr3t-pass" || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("expected bcrypt hash, got %q", hash)
	}
	if !VerifyPassword(hash, "s3cr3t-pass") {
		t.Fatalf("VerifyPassword returned false for matching password")
	}
	if VerifyPassword(hash, "wrong-pass") {
		t.Fatalf("VerifyPassword returned true for wrong password")
	}
	if VerifyPassword("", "s3cr3t-pass") {
		t.Fatalf("VerifyPassword returned true for empty hash")
	}

	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:", "TestServer")
	if err != nil {
		t.Fatalf("db.Open(:memory:) failed: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("database.Close() failed: %v", err)
		}
	}()

	u1, err := EnsureAdminUser(ctx, database, "Admin", "initial-pass")
	if err != nil {
		t.Fatalf("EnsureAdminUser create failed: %v", err)
	}
	wantID := idgen.DeterministicHex32("user:admin")
	if u1.ID != wantID {
		t.Fatalf("admin ID = %q, want %q", u1.ID, wantID)
	}
	if !u1.IsAdmin || u1.Username != "Admin" {
		t.Fatalf("unexpected admin user: %+v", u1)
	}
	if !VerifyPassword(u1.PasswordHash, "initial-pass") {
		t.Fatalf("created admin password did not verify")
	}

	// Second call should return existing user without error or overwriting password.
	u2, err := EnsureAdminUser(ctx, database, "Admin", "different-pass")
	if err != nil {
		t.Fatalf("EnsureAdminUser existing failed: %v", err)
	}
	if u2.ID != u1.ID || u2.PasswordHash != u1.PasswordHash {
		t.Fatalf("expected existing user unchanged, got %+v vs %+v", u2, u1)
	}
}

func TestAuthMiddlewareWithInMemoryDB(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:", "TestServer")
	if err != nil {
		t.Fatalf("db.Open(:memory:) failed: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("database.Close() failed: %v", err)
		}
	}()

	adminUser, err := EnsureAdminUser(ctx, database, "admin", "admin-pass")
	if err != nil {
		t.Fatalf("EnsureAdminUser failed: %v", err)
	}

	regularHash, err := HashPassword("user-pass")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	regularUser := db.ProxyUser{
		ID:           idgen.NewRandomHex32(),
		Username:     "viewer",
		PasswordHash: regularHash,
		IsAdmin:      false,
	}
	if err := database.CreateUser(ctx, regularUser); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	adminToken := idgen.NewRandomHex32()
	if err := database.UpsertSession(ctx, db.ProxySession{
		Token:      adminToken,
		UserID:     adminUser.ID,
		DeviceID:   "dev-admin",
		ClientName: "Web",
		DeviceName: "Browser",
		AppVersion: "12.4.0",
	}); err != nil {
		t.Fatalf("UpsertSession admin failed: %v", err)
	}

	userToken := idgen.NewRandomHex32()
	if err := database.UpsertSession(ctx, db.ProxySession{
		Token:      userToken,
		UserID:     regularUser.ID,
		DeviceID:   "dev-user",
		ClientName: "Mobile",
		DeviceName: "Phone",
		AppVersion: "1.0",
	}); err != nil {
		t.Fatalf("UpsertSession user failed: %v", err)
	}

	// Verify Context helpers directly.
	if _, ok := FromContext(context.Background()); ok {
		t.Fatalf("expected FromContext to return false on empty context")
	}

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ac, ok := FromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("anonymous"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ac.User.Username + ":" + ac.Client.Client))
	})

	// Authenticate: missing token -> 401 JSON.
	authHandler := Authenticate(database)(okHandler)
	rec := httptest.NewRecorder()
	authHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/Users/Me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Authenticate missing token status = %d, want 401", rec.Code)
	}
	var errBody map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil || errBody["error"] != "Unauthorized" {
		t.Fatalf("unexpected 401 body: %q", rec.Body.String())
	}

	// Authenticate: invalid token -> 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	req.Header.Set("X-Emby-Token", "nonexistent-token")
	authHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Authenticate invalid token status = %d, want 401", rec.Code)
	}

	// Authenticate: valid token -> 200 and context populated.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	req.Header.Set("Authorization", `MediaBrowser Client="TestClient", Token="`+userToken+`"`)
	authHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "viewer:TestClient" {
		t.Fatalf("Authenticate valid token = (%d, %q), want (200, %q)", rec.Code, rec.Body.String(), "viewer:TestClient")
	}

	// RequireAdmin: unauthenticated -> 401.
	adminHandler := RequireAdmin(database)(okHandler)
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("RequireAdmin missing token status = %d, want 401", rec.Code)
	}

	// RequireAdmin: non-admin -> 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	req.Header.Set("X-Emby-Token", userToken)
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("RequireAdmin non-admin status = %d, want 403", rec.Code)
	}

	// RequireAdmin: admin cookie -> 200.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	req.AddCookie(&http.Cookie{Name: "mergerfin_admin_token", Value: adminToken})
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "admin:") {
		t.Fatalf("RequireAdmin admin cookie = (%d, %q), want (200, admin:...)", rec.Code, rec.Body.String())
	}

	// OptionalUser: anonymous -> 200 anonymous.
	optHandler := OptionalUser(database)(okHandler)
	rec = httptest.NewRecorder()
	optHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/Items", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "anonymous" {
		t.Fatalf("OptionalUser anonymous = (%d, %q), want (200, anonymous)", rec.Code, rec.Body.String())
	}

	// OptionalUser: valid token -> 200 with user.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/Items?api_key="+adminToken, nil)
	optHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "admin:") {
		t.Fatalf("OptionalUser valid = (%d, %q), want (200, admin:...)", rec.Code, rec.Body.String())
	}

	// Confirm TouchSession updated or kept valid LastSeenAt.
	sess, err := database.GetSessionByToken(ctx, adminToken)
	if err != nil || sess.LastSeenAt.IsZero() || time.Since(sess.LastSeenAt) > time.Minute {
		t.Fatalf("unexpected session after TouchSession: sess=%+v err=%v", sess, err)
	}
}
