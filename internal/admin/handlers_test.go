package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

type mockSyncer struct {
	mu       sync.Mutex
	status   SyncStatus
	calls    int
	notifyCh chan struct{}
}

func (m *mockSyncer) TriggerSync(ctx context.Context) error {
	m.mu.Lock()
	m.calls++
	now := time.Now().UTC()
	m.status = SyncStatus{
		Running:      false,
		LastSyncAt:   &now,
		ItemsIndexed: 42,
	}
	ch := m.notifyCh
	m.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

func (m *mockSyncer) Status() SyncStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func setupAdminTest(t *testing.T, syncer SyncTrigger) (*db.DB, *upstream.Pool, chi.Router, string) {
	t.Helper()
	ctx := context.Background()

	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"), "MergerFin-Admin-Test")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	adminUser, err := auth.EnsureAdminUser(ctx, database, "admin", "adminpass")
	if err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}

	// Create a non-admin regular user as well.
	userHash, err := auth.HashPassword("userpass")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := database.CreateUser(ctx, db.ProxyUser{
		ID:           idgen.DeterministicHex32("user:viewer"),
		Username:     "viewer",
		PasswordHash: userHash,
		IsAdmin:      false,
	}); err != nil {
		t.Fatalf("CreateUser viewer: %v", err)
	}

	pool, err := upstream.NewPool(database, "test-admin-secret", nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	h := NewHandler(database, pool, syncer, nil)
	r := chi.NewRouter()
	h.Mount(r)
	return database, pool, r, adminUser.ID
}

func doReq(t *testing.T, r http.Handler, method, path string, body any, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func loginAdmin(t *testing.T, r http.Handler) []*http.Cookie {
	t.Helper()
	rec := doReq(t, r, http.MethodPost, "/admin/api/login", map[string]string{
		"username": "admin",
		"password": "adminpass",
	}, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login failed: %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == adminCookieName {
			found = c
		}
	}
	if found == nil || found.Value == "" || !found.HttpOnly || found.Path != "/" {
		t.Fatalf("expected HttpOnly %s cookie with Path=/, got %+v", adminCookieName, cookies)
	}
	return cookies
}

func TestAdminUIEmbeddedAndOffline(t *testing.T) {
	_, _, r, _ := setupAdminTest(t, nil)

	rec := doReq(t, r, http.MethodGet, "/admin", nil, nil, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("expected 302 to /admin/, got %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}

	uiRec := doReq(t, r, http.MethodGet, "/admin/", nil, nil, nil)
	if uiRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /admin/, got %d", uiRec.Code)
	}
	html := uiRec.Body.String()
	if !strings.Contains(html, "MergerFin Admin") {
		t.Fatalf("expected MergerFin Admin in HTML")
	}
	lower := strings.ToLower(html)
	for _, ext := range []string{"<script src=", "<link ", "cdn.", "unpkg.com", "jsdelivr.net", "fonts.googleapis"} {
		if strings.Contains(lower, ext) {
			t.Fatalf("embedded UI must not contain external asset %q", ext)
		}
	}
}

func TestAdminAuthEnforcementAndLoginLogout(t *testing.T) {
	database, _, r, _ := setupAdminTest(t, nil)

	// Unauthenticated endpoints must return 401.
	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/api/me"},
		{http.MethodGet, "/admin/api/status"},
		{http.MethodGet, "/admin/api/servers"},
		{http.MethodGet, "/admin/api/users"},
		{http.MethodPost, "/admin/api/sync"},
	} {
		rec := doReq(t, r, ep.method, ep.path, nil, nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth: got %d, want 401", ep.method, ep.path, rec.Code)
		}
	}

	// Non-admin user login must return 403.
	nonAdminLogin := doReq(t, r, http.MethodPost, "/admin/api/login", map[string]string{
		"username": "viewer",
		"password": "userpass",
	}, nil, nil)
	if nonAdminLogin.Code != http.StatusForbidden {
		t.Fatalf("non-admin login: got %d, want 403", nonAdminLogin.Code)
	}

	// Non-admin session token against RequireAdmin must return 403.
	viewerTok := idgen.NewRandomHex32()
	if err := database.UpsertSession(context.Background(), db.ProxySession{
		Token:  viewerTok,
		UserID: idgen.DeterministicHex32("user:viewer"),
	}); err != nil {
		t.Fatalf("UpsertSession viewer: %v", err)
	}
	forbiddenRec := doReq(t, r, http.MethodGet, "/admin/api/servers", nil, nil, map[string]string{
		"X-Emby-Token": viewerTok,
	})
	if forbiddenRec.Code != http.StatusForbidden {
		t.Fatalf("non-admin token on /admin/api/servers: got %d, want 403", forbiddenRec.Code)
	}

	// Admin login succeeds and sets cookie.
	cookies := loginAdmin(t, r)
	meRec := doReq(t, r, http.MethodGet, "/admin/api/me", nil, cookies, nil)
	if meRec.Code != http.StatusOK {
		t.Fatalf("/admin/api/me: got %d %s", meRec.Code, meRec.Body.String())
	}

	// Logout invalidates session.
	logoutRec := doReq(t, r, http.MethodPost, "/admin/api/logout", nil, cookies, nil)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("/admin/api/logout: got %d", logoutRec.Code)
	}
	afterLogout := doReq(t, r, http.MethodGet, "/admin/api/me", nil, cookies, nil)
	if afterLogout.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", afterLogout.Code)
	}
}

func TestAdminServersCRUDAndNoTokenLeak(t *testing.T) {
	_, pool, r, _ := setupAdminTest(t, nil)
	cookies := loginAdmin(t, r)

	const secretUpstreamToken = "secret-upstream-token-never-leak"
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ServerName": "Mock Upstream",
				"Version":    "10.9.11",
			})
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"User":        map[string]any{"Id": "upstream-uid-1"},
				"AccessToken": secretUpstreamToken,
			})
		default:
			http.NotFound(w, req)
		}
	}))
	defer jf.Close()

	// Test connection endpoint.
	testRec := doReq(t, r, http.MethodPost, "/admin/api/servers/test", map[string]any{
		"baseUrl":  jf.URL,
		"username": "svc",
		"password": "pw",
	}, cookies, nil)
	if testRec.Code != http.StatusOK {
		t.Fatalf("servers/test: got %d %s", testRec.Code, testRec.Body.String())
	}
	if strings.Contains(testRec.Body.String(), secretUpstreamToken) || strings.Contains(testRec.Body.String(), "accessToken") {
		t.Fatalf("servers/test leaked AccessToken: %s", testRec.Body.String())
	}

	// Create server.
	createRec := doReq(t, r, http.MethodPost, "/admin/api/servers", map[string]any{
		"name":       "Home NAS",
		"baseUrl":    jf.URL,
		"username":   "svc",
		"password":   "pw",
		"priority":   50,
		"streamMode": "proxy",
		"enabled":    true,
	}, cookies, nil)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create server: got %d %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created server: %v", err)
	}

	// List servers and verify AccessToken is never leaked.
	listRec := doReq(t, r, http.MethodGet, "/admin/api/servers", nil, cookies, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list servers: got %d", listRec.Code)
	}
	listBody := listRec.Body.String()
	for _, forbidden := range []string{secretUpstreamToken, "accessToken", "AccessToken", "access_token", "enc:"} {
		if strings.Contains(listBody, forbidden) {
			t.Fatalf("GET /admin/api/servers leaked %q in body: %s", forbidden, listBody)
		}
	}

	// Update server without password keeps encrypted token intact.
	updRec := doReq(t, r, http.MethodPut, "/admin/api/servers/"+created.ID, map[string]any{
		"name":       "Home NAS Renamed",
		"baseUrl":    jf.URL,
		"username":   "svc",
		"password":   "",
		"priority":   10,
		"streamMode": "redirect",
		"enabled":    false,
	}, cookies, nil)
	if updRec.Code != http.StatusOK {
		t.Fatalf("update server: got %d %s", updRec.Code, updRec.Body.String())
	}
	decSrv, err := pool.GetDecryptedServer(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetDecryptedServer: %v", err)
	}
	if decSrv.AccessToken != secretUpstreamToken || decSrv.StreamMode != "redirect" || decSrv.Enabled {
		t.Fatalf("unexpected server after update: %+v", decSrv)
	}

	// Delete server.
	delRec := doReq(t, r, http.MethodDelete, "/admin/api/servers/"+created.ID, nil, cookies, nil)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete server: got %d %s", delRec.Code, delRec.Body.String())
	}
}

func TestAdminUsersCRUDAndSelfDeleteGuard(t *testing.T) {
	_, _, r, adminID := setupAdminTest(t, nil)
	cookies := loginAdmin(t, r)

	// List users: must not leak passwordHash.
	listRec := doReq(t, r, http.MethodGet, "/admin/api/users", nil, cookies, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list users: got %d", listRec.Code)
	}
	if strings.Contains(listRec.Body.String(), "passwordHash") || strings.Contains(listRec.Body.String(), "$2a$") {
		t.Fatalf("users list leaked password hash: %s", listRec.Body.String())
	}

	// Create new user.
	createRec := doReq(t, r, http.MethodPost, "/admin/api/users", map[string]any{
		"username": "bob",
		"password": "bobpassword",
		"isAdmin":  false,
	}, cookies, nil)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create user: got %d %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	// Duplicate username -> 409.
	dupRec := doReq(t, r, http.MethodPost, "/admin/api/users", map[string]any{
		"username": "bob",
		"password": "bobpassword",
		"isAdmin":  false,
	}, cookies, nil)
	if dupRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate user, got %d", dupRec.Code)
	}

	// Prevent deleting self.
	selfDelRec := doReq(t, r, http.MethodDelete, "/admin/api/users/"+adminID, nil, cookies, nil)
	if selfDelRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 preventing self-delete, got %d %s", selfDelRec.Code, selfDelRec.Body.String())
	}

	// Delete created user.
	delRec := doReq(t, r, http.MethodDelete, "/admin/api/users/"+created.ID, nil, cookies, nil)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete user: got %d %s", delRec.Code, delRec.Body.String())
	}
}

func TestAdminStatusAndSync(t *testing.T) {
	// Nil syncer returns initializing JSON.
	_, _, rNil, _ := setupAdminTest(t, nil)
	cookiesNil := loginAdmin(t, rNil)
	syncNilRec := doReq(t, rNil, http.MethodPost, "/admin/api/sync", nil, cookiesNil, nil)
	if syncNilRec.Code != http.StatusOK || !strings.Contains(syncNilRec.Body.String(), "sync engine initializing") {
		t.Fatalf("expected sync initializing response, got %d %s", syncNilRec.Code, syncNilRec.Body.String())
	}

	// With mock syncer: triggers async and updates status.
	ms := &mockSyncer{notifyCh: make(chan struct{}, 1)}
	_, _, r, _ := setupAdminTest(t, ms)
	cookies := loginAdmin(t, r)

	syncRec := doReq(t, r, http.MethodPost, "/admin/api/sync", nil, cookies, nil)
	if syncRec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d %s", syncRec.Code, syncRec.Body.String())
	}

	select {
	case <-ms.notifyCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for TriggerSync to execute")
	}

	stRec := doReq(t, r, http.MethodGet, "/admin/api/status", nil, cookies, nil)
	if stRec.Code != http.StatusOK {
		t.Fatalf("status: got %d %s", stRec.Code, stRec.Body.String())
	}
	var st statusResponse
	if err := json.Unmarshal(stRec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.ServerName != "MergerFin-Admin-Test" || st.ServerID == "" || st.SyncStatus.ItemsIndexed != 42 || st.Counts.Users != 2 {
		t.Fatalf("unexpected status response: %+v", st)
	}
}
