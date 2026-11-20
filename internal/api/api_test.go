package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/tsrz/mergerfin/internal/admin"
	"github.com/tsrz/mergerfin/internal/api"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/config"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/upstream"
)

type testEnv struct {
	db     *db.DB
	wsHub  *api.WSHub
	router http.Handler
	admin  *db.ProxyUser
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()

	database, err := db.Open(ctx, ":memory:", "MergerFin Test")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	adminUser, err := auth.EnsureAdminUser(ctx, database, "admin", "secretpass")
	if err != nil {
		t.Fatalf("auth.EnsureAdminUser: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := upstream.NewPool(database, "test-secret-key", logger)
	if err != nil {
		t.Fatalf("upstream.NewPool: %v", err)
	}

	wsHub := api.NewWSHub(database, logger)
	t.Cleanup(wsHub.CloseAll)

	adminHandler := admin.NewHandler(database, pool, nil, logger)
	cfg := &config.Config{
		Port:       "8096",
		ServerName: "MergerFin Test",
		AdminUser:  "admin",
		AdminPass:  "secretpass",
	}

	router := api.NewRouter(api.RouterDeps{
		Config: cfg,
		DB:     database,
		Pool:   pool,
		Admin:  adminHandler,
		WSHub:  wsHub,
		Syncer: nil,
		Logger: logger,
	})

	return &testEnv{
		db:     database,
		wsHub:  wsHub,
		router: router,
		admin:  adminUser,
	}
}

func insertDashesUUID(hex32 string) string {
	return hex32[0:8] + "-" + hex32[8:12] + "-" + hex32[12:16] + "-" + hex32[16:20] + "-" + hex32[20:32]
}

func assertHex32(t *testing.T, id string) {
	t.Helper()
	if len(id) != 32 || strings.Contains(id, "-") {
		t.Fatalf("expected 32-char hex ID without dashes, got %q", id)
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("expected lowercase hex ID, got %q", id)
		}
	}
}

func authenticateTestUser(t *testing.T, router http.Handler) api.AuthenticationResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"Username": "admin",
		"Pw":       "secretpass",
	})
	req := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", `MediaBrowser Client="Jellyfin Web", Device="Firefox", DeviceId="dev-123", Version="10.9.0"`)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("AuthenticateByName status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var res api.AuthenticationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode AuthenticationResult: %v", err)
	}
	return res
}

func TestLegacyPrefixStripping(t *testing.T) {
	env := newTestEnv(t)

	// /emby/System/Info/Public
	req := httptest.NewRequest(http.MethodGet, "/emby/System/Info/Public", nil)
	req.Host = "media.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /emby/System/Info/Public status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var pub struct {
		LocalAddress           string `json:"LocalAddress"`
		ServerName             string `json:"ServerName"`
		Version                string `json:"Version"`
		ProductName            string `json:"ProductName"`
		OperatingSystem        string `json:"OperatingSystem"`
		Id                     string `json:"Id"`
		StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode public info: %v", err)
	}
	if pub.LocalAddress != "https://media.example.com" {
		t.Fatalf("LocalAddress = %q, want https://media.example.com", pub.LocalAddress)
	}
	if pub.Version != "12.4.0" || pub.ProductName != "Jellyfin Server" || !pub.StartupWizardCompleted {
		t.Fatalf("unexpected public info: %+v", pub)
	}
	assertHex32(t, pub.Id)

	// /mediabrowser/System/Ping (case-insensitive)
	req2 := httptest.NewRequest(http.MethodGet, "/MediaBrowser/System/Ping", nil)
	rec2 := httptest.NewRecorder()
	env.router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || rec2.Body.String() != "Jellyfin Server" {
		t.Fatalf("GET /MediaBrowser/System/Ping = %d %q", rec2.Code, rec2.Body.String())
	}
}

func TestAuthenticateByNameAndUserRoutes(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	assertHex32(t, authRes.AccessToken)
	assertHex32(t, authRes.ServerId)
	assertHex32(t, authRes.User.Id)
	assertHex32(t, authRes.SessionInfo.Id)
	assertHex32(t, authRes.SessionInfo.UserId)

	if authRes.User.Name != "admin" || !authRes.User.Policy.IsAdministrator {
		t.Fatalf("unexpected user dto: %+v", authRes.User)
	}
	if authRes.SessionInfo.Client != "Jellyfin Web" ||
		authRes.SessionInfo.DeviceName != "Firefox" ||
		authRes.SessionInfo.DeviceId != "dev-123" ||
		authRes.SessionInfo.ApplicationVersion != "10.9.0" {
		t.Fatalf("client metadata not extracted properly: %+v", authRes.SessionInfo)
	}
	if authRes.SessionInfo.PlayState == nil || authRes.SessionInfo.Capabilities == nil {
		t.Fatalf("expected non-nil PlayState and Capabilities in SessionInfo")
	}

	// Unauthenticated /Users/Me must fail with 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	unauthRec := httptest.NewRecorder()
	env.router.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /Users/Me status = %d, want 401", unauthRec.Code)
	}

	// Authenticated /Users/Me
	meReq := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	meReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	meRec := httptest.NewRecorder()
	env.router.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("GET /Users/Me status = %d, body = %s", meRec.Code, meRec.Body.String())
	}
	var me api.UserDto
	if err := json.Unmarshal(meRec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /Users/Me: %v", err)
	}
	if me.Id != authRes.User.Id || me.Name != "admin" {
		t.Fatalf("unexpected /Users/Me: %+v", me)
	}

	// /Users/{dashed-uuid} normalization
	dashed := insertDashesUUID(authRes.User.Id)
	byIDReq := httptest.NewRequest(http.MethodGet, "/Users/"+dashed, nil)
	byIDReq.Header.Set("X-MediaBrowser-Token", authRes.AccessToken)
	byIDRec := httptest.NewRecorder()
	env.router.ServeHTTP(byIDRec, byIDReq)
	if byIDRec.Code != http.StatusOK {
		t.Fatalf("GET /Users/{dashed} status = %d, body = %s", byIDRec.Code, byIDRec.Body.String())
	}
	var byID api.UserDto
	if err := json.Unmarshal(byIDRec.Body.Bytes(), &byID); err != nil {
		t.Fatalf("decode /Users/{dashed}: %v", err)
	}
	if byID.Id != authRes.User.Id {
		t.Fatalf("normalized ID mismatch: got %q, want %q", byID.Id, authRes.User.Id)
	}
}

func TestDisplayPreferencesRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	// Initial GET returns defaults
	getReq := httptest.NewRequest(http.MethodGet, "/DisplayPreferences/usersettings?client=emby", nil)
	getReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	getRec := httptest.NewRecorder()
	env.router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /DisplayPreferences/usersettings status = %d", getRec.Code)
	}
	var initial api.DisplayPreferencesDto
	if err := json.Unmarshal(getRec.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode initial DisplayPreferences: %v", err)
	}
	if initial.SortBy != "SortName" || initial.ViewType != "posters" || initial.CustomPrefs == nil {
		t.Fatalf("unexpected defaults: %+v", initial)
	}

	// POST update
	payload, _ := json.Marshal(map[string]any{
		"Id":       "usersettings",
		"ViewType": "list",
		"SortBy":   "DateCreated",
		"CustomPrefs": map[string]string{
			"homeSection0": "latestmedia",
		},
		"ShowBackdrop": false,
	})
	postReq := httptest.NewRequest(http.MethodPost, "/DisplayPreferences/usersettings?client=emby", bytes.NewReader(payload))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	postRec := httptest.NewRecorder()
	env.router.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusNoContent {
		t.Fatalf("POST /DisplayPreferences/usersettings status = %d, body = %s", postRec.Code, postRec.Body.String())
	}

	// Second GET reflects persisted changes
	getReq2 := httptest.NewRequest(http.MethodGet, "/DisplayPreferences/usersettings?client=emby", nil)
	getReq2.Header.Set("X-Emby-Token", authRes.AccessToken)
	getRec2 := httptest.NewRecorder()
	env.router.ServeHTTP(getRec2, getReq2)
	if getRec2.Code != http.StatusOK {
		t.Fatalf("second GET status = %d", getRec2.Code)
	}
	var updated api.DisplayPreferencesDto
	if err := json.Unmarshal(getRec2.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated DisplayPreferences: %v", err)
	}
	if updated.ViewType != "list" || updated.SortBy != "DateCreated" || updated.ShowBackdrop != false || updated.CustomPrefs["homeSection0"] != "latestmedia" {
		t.Fatalf("unexpected updated DisplayPreferences: %+v", updated)
	}
}

func TestSystemInfoAndUtilityEndpoints(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	// Authenticated /System/Info
	req := httptest.NewRequest(http.MethodGet, "/System/Info", nil)
	req.Header.Set("X-Emby-Token", authRes.AccessToken)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /System/Info status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode /System/Info: %v", err)
	}
	if info["Version"] != "12.4.0" || info["SystemArchitecture"] != "X64" || info["SupportsLibraryMonitor"] != true {
		t.Fatalf("unexpected /System/Info payload: %+v", info)
	}

	// BitrateTest
	brReq := httptest.NewRequest(http.MethodGet, "/Playback/BitrateTest?Size=1234", nil)
	brRec := httptest.NewRecorder()
	env.router.ServeHTTP(brRec, brReq)
	if brRec.Code != http.StatusOK || brRec.Body.Len() != 1234 {
		t.Fatalf("BitrateTest = %d len=%d", brRec.Code, brRec.Body.Len())
	}

	// QuickConnect / Branding / Localization
	qcReq := httptest.NewRequest(http.MethodGet, "/QuickConnect/Enabled", nil)
	qcRec := httptest.NewRecorder()
	env.router.ServeHTTP(qcRec, qcReq)
	if qcRec.Code != http.StatusOK || strings.TrimSpace(qcRec.Body.String()) != "false" {
		t.Fatalf("QuickConnect = %d %q", qcRec.Code, qcRec.Body.String())
	}
}

func TestWebSocketHandshakeAndKeepAlive(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	srv := httptest.NewServer(env.router)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/socket?api_key=" + authRes.AccessToken
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.CloseNow()

	type wsMsg struct {
		MessageType string `json:"MessageType"`
		Data        any    `json:"Data"`
	}

	// Expect initial ForceKeepAlive + KeepAlive
	var seenForce, seenKeepAlive bool
	for i := 0; i < 2; i++ {
		var msg wsMsg
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("wsjson.Read #%d: %v", i, err)
		}
		switch msg.MessageType {
		case "ForceKeepAlive":
			seenForce = true
		case "KeepAlive":
			seenKeepAlive = true
		}
	}
	if !seenForce || !seenKeepAlive {
		t.Fatalf("expected ForceKeepAlive and KeepAlive on connect, got force=%v keepAlive=%v", seenForce, seenKeepAlive)
	}

	// Send KeepAlive and expect KeepAlive reply
	if err := wsjson.Write(ctx, conn, wsMsg{MessageType: "KeepAlive", Data: ""}); err != nil {
		t.Fatalf("wsjson.Write KeepAlive: %v", err)
	}

	var reply wsMsg
	if err := wsjson.Read(ctx, conn, &reply); err != nil {
		t.Fatalf("wsjson.Read reply: %v", err)
	}
	if reply.MessageType != "KeepAlive" {
		t.Fatalf("expected KeepAlive reply, got %+v", reply)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestSessionLogoutCapabilitiesAndWebStatic(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	// Public users list
	pubReq := httptest.NewRequest(http.MethodGet, "/Users/Public", nil)
	pubRec := httptest.NewRecorder()
	env.router.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusOK {
		t.Fatalf("GET /Users/Public status = %d", pubRec.Code)
	}
	var pubUsers []api.UserDto
	if err := json.Unmarshal(pubRec.Body.Bytes(), &pubUsers); err != nil {
		t.Fatalf("decode /Users/Public: %v", err)
	}
	if len(pubUsers) != 1 || pubUsers[0].Name != "admin" {
		t.Fatalf("unexpected /Users/Public: %+v", pubUsers)
	}

	// Capabilities & Sessions
	for _, path := range []string{"/Sessions/Capabilities", "/Sessions/Capabilities/Full"} {
		capReq := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		capReq.Header.Set("X-Emby-Token", authRes.AccessToken)
		capRec := httptest.NewRecorder()
		env.router.ServeHTTP(capRec, capReq)
		if capRec.Code != http.StatusNoContent {
			t.Fatalf("POST %s status = %d", path, capRec.Code)
		}
	}

	sessReq := httptest.NewRequest(http.MethodGet, "/Sessions", nil)
	sessReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	sessRec := httptest.NewRecorder()
	env.router.ServeHTTP(sessRec, sessReq)
	if sessRec.Code != http.StatusOK {
		t.Fatalf("GET /Sessions status = %d", sessRec.Code)
	}

	// Logout invalidates token
	loReq := httptest.NewRequest(http.MethodPost, "/Sessions/Logout", nil)
	loReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	loRec := httptest.NewRecorder()
	env.router.ServeHTTP(loRec, loReq)
	if loRec.Code != http.StatusNoContent {
		t.Fatalf("POST /Sessions/Logout status = %d", loRec.Code)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	meReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	meRec := httptest.NewRecorder()
	env.router.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /Users/Me after logout = %d, want 401", meRec.Code)
	}

	// Static web serving & root redirect
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<h1>MergerFin Web</h1>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "app.js"), []byte("console.log('ok');"), 0o644); err != nil {
		t.Fatalf("write app.js: %v", err)
	}
	webRouter := api.NewRouter(api.RouterDeps{
		Config: &config.Config{Port: "8096", ServerName: "MergerFin", WebDir: webDir},
		DB:     env.db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	redirReq := httptest.NewRequest(http.MethodGet, "/", nil)
	redirRec := httptest.NewRecorder()
	webRouter.ServeHTTP(redirRec, redirReq)
	if redirRec.Code != http.StatusFound || redirRec.Header().Get("Location") != "/web/index.html" {
		t.Fatalf("GET / redirect = %d location=%q", redirRec.Code, redirRec.Header().Get("Location"))
	}

	indexReq := httptest.NewRequest(http.MethodGet, "/web/", nil)
	indexRec := httptest.NewRecorder()
	webRouter.ServeHTTP(indexRec, indexReq)
	if indexRec.Code != http.StatusOK || !strings.Contains(indexRec.Body.String(), "MergerFin Web") {
		t.Fatalf("GET /web/ = %d body=%q", indexRec.Code, indexRec.Body.String())
	}

	assetReq := httptest.NewRequest(http.MethodGet, "/web/app.js", nil)
	assetRec := httptest.NewRecorder()
	webRouter.ServeHTTP(assetRec, assetReq)
	if assetRec.Code != http.StatusOK || !strings.Contains(assetRec.Body.String(), "console.log('ok')") {
		t.Fatalf("GET /web/app.js = %d body=%q", assetRec.Code, assetRec.Body.String())
	}
}
