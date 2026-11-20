package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/config"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

func setupPlaybackTestRouter(t *testing.T) (http.Handler, *db.DB, *upstream.Pool, string, string) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:", "PlaybackTest")
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	adminUser, err := auth.EnsureAdminUser(ctx, database, "admin", "admin-pass")
	if err != nil {
		t.Fatalf("EnsureAdminUser failed: %v", err)
	}
	proxyToken := idgen.NewRandomHex32()
	if err := database.UpsertSession(ctx, db.ProxySession{
		Token:      proxyToken,
		UserID:     adminUser.ID,
		DeviceID:   "client-dev-1",
		ClientName: "Jellyfin Web",
		DeviceName: "Browser",
		AppVersion: "12.4.0",
	}); err != nil {
		t.Fatalf("UpsertSession failed: %v", err)
	}

	pool, err := upstream.NewPool(database, "test-secret", slog.Default())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}

	cfg := &config.Config{Port: "8096", ServerName: "PlaybackTest"}
	h := newAPIHandler(cfg, database, pool, upstream.NewClient(), slog.Default())

	r := chi.NewRouter()
	r.Group(func(ar chi.Router) {
		ar.Use(auth.Authenticate(database))
		ar.Post("/Items/{itemId}/PlaybackInfo", h.handlePlaybackInfo)
		ar.Get("/Items/{itemId}/PlaybackInfo", h.handlePlaybackInfo)
		ar.Delete("/Videos/ActiveEncodings", h.handleStopActiveEncodings)
	})
	return r, database, pool, adminUser.ID, proxyToken
}

func TestCleanContainerExt(t *testing.T) {
	cases := []struct {
		container string
		path      string
		want      string
	}{
		{"mov,mp4,m4a,3gp,3g2,mj2", "", "mp4"},
		{"matroska,webm", "", "mkv"},
		{"mpegts", "", "ts"},
		{"", "/media/movie.mkv", "mkv"},
		{"", "", "mp4"},
	}
	for _, tc := range cases {
		if got := cleanContainerExt(tc.container, tc.path); got != tc.want {
			t.Fatalf("cleanContainerExt(%q, %q) = %q, want %q", tc.container, tc.path, got, tc.want)
		}
	}
}

func TestPlaybackInfoNegotiationScoringAndActiveEncodings(t *testing.T) {
	handler, database, pool, _, proxyToken := setupPlaybackTestRouter(t)
	ctx := context.Background()

	var (
		mu          sync.Mutex
		stopCalls   []url.Values
	)

	// Upstream A (priority 10): DirectStream only (no DirectPlay), supports transcoding, mov,mp4 container.
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": "UpA", "Version": "12.4.0"})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"User": map[string]any{"Id": "ua"}, "AccessToken": "tok-a"})
		case r.Method == http.MethodPost && r.URL.Path == "/Items/remote-a/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"PlaySessionId": "remote-ps-a-fallback",
				"MediaSources": []map[string]any{
					{
						"Id":                     "ms-a",
						"Container":              "mov,mp4,m4a,3gp,3g2,mj2",
						"Path":                   "/media/interstellar.mp4",
						"Bitrate":                8000000,
						"SupportsDirectPlay":     false,
						"SupportsDirectStream":   true,
						"SupportsTranscoding":    true,
						"TranscodingUrl":         "/videos/remote-a/master.m3u8?DeviceId=d1&PlaySessionId=remote-ps-a-hls&api_key=tok-a",
						"TranscodingSubProtocol": "hls",
					},
				},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/Videos/ActiveEncodings":
			mu.Lock()
			stopCalls = append(stopCalls, r.URL.Query())
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upA.Close()

	// Upstream B (priority 20): DirectPlay = true (should outrank DirectStream on A!), matroska,webm container.
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": "UpB", "Version": "12.4.0"})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"User": map[string]any{"Id": "ub"}, "AccessToken": "tok-b"})
		case r.Method == http.MethodPost && r.URL.Path == "/Items/remote-b/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"PlaySessionId": "remote-ps-b",
				"MediaSources": []map[string]any{
					{
						"Id":                   "ms-b",
						"Container":            "matroska,webm",
						"Bitrate":              4000000,
						"SupportsDirectPlay":   true,
						"SupportsDirectStream": true,
						"SupportsTranscoding":  false,
					},
				},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/Videos/ActiveEncodings":
			mu.Lock()
			stopCalls = append(stopCalls, r.URL.Query())
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upB.Close()

	srvA, err := pool.AddOrUpdateServer(ctx, "", "UpA", upA.URL, "admin", "pass", 10, "proxy", true)
	if err != nil {
		t.Fatalf("add srvA: %v", err)
	}
	srvB, err := pool.AddOrUpdateServer(ctx, "", "UpB", upB.URL, "admin", "pass", 20, "proxy", true)
	if err != nil {
		t.Fatalf("add srvB: %v", err)
	}

	itemID := idgen.VirtualItemID("movie:tmdb:157336")
	if err := database.UpsertItem(ctx, db.VirtualItem{
		ID:           itemID,
		LibraryID:    idgen.MoviesLibraryID(),
		ParentID:     idgen.MoviesLibraryID(),
		ItemType:     "Movie",
		CanonicalKey: "movie:tmdb:157336",
		Name:         "Interstellar",
	}); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	srcAID := idgen.SourceID(srvA.ID, "ms-a")
	srcBID := idgen.SourceID(srvB.ID, "ms-b")
	if err := database.UpsertSource(ctx, db.ItemSource{
		SourceID: srcAID, VirtualItemID: itemID, ServerID: srvA.ID,
		RemoteItemID: "remote-a", RemoteMediaSourceID: "ms-a", Label: "[UpA] 1080p",
		Container: "mp4", Bitrate: 8000000,
	}); err != nil {
		t.Fatalf("UpsertSource A: %v", err)
	}
	if err := database.UpsertSource(ctx, db.ItemSource{
		SourceID: srcBID, VirtualItemID: itemID, ServerID: srvB.ID,
		RemoteItemID: "remote-b", RemoteMediaSourceID: "ms-b", Label: "[UpB] 720p",
		Container: "mkv", Bitrate: 4000000,
	}); err != nil {
		t.Fatalf("UpsertSource B: %v", err)
	}

	// 1. Negotiate without MediaSourceId: DirectPlay (srcBID) must rank first!
	req := httptest.NewRequest(http.MethodPost, "/Items/"+itemID+"/PlaybackInfo", bytes.NewBufferString(`{"DeviceId":"tv-1","MaxStreamingBitrate":20000000}`))
	req.Header.Set("X-Emby-Token", proxyToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PlaybackInfo status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		MediaSources  []map[string]any `json:"MediaSources"`
		PlaySessionId string           `json:"PlaySessionId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal PlaybackInfo: %v", err)
	}
	if len(resp.MediaSources) != 2 {
		t.Fatalf("expected 2 MediaSources, got %d", len(resp.MediaSources))
	}
	if resp.MediaSources[0]["Id"] != srcBID || resp.MediaSources[1]["Id"] != srcAID {
		t.Fatalf("expected DirectPlay source %s first then %s, got %v, %v", srcBID, srcAID, resp.MediaSources[0]["Id"], resp.MediaSources[1]["Id"])
	}

	// Verify DirectStreamUrl container normalization & TranscodingUrl rewriting without upstream token leak.
	dsB, _ := resp.MediaSources[0]["DirectStreamUrl"].(string)
	if !strings.Contains(dsB, "/Videos/"+itemID+"/stream.mkv?") || !strings.Contains(dsB, "api_key="+proxyToken) {
		t.Fatalf("unexpected DirectStreamUrl for B: %q", dsB)
	}
	dsA, _ := resp.MediaSources[1]["DirectStreamUrl"].(string)
	if !strings.Contains(dsA, "/Videos/"+itemID+"/stream.mp4?") {
		t.Fatalf("unexpected DirectStreamUrl for A: %q", dsA)
	}
	tsA, _ := resp.MediaSources[1]["TranscodingUrl"].(string)
	if !strings.Contains(tsA, "/Videos/"+itemID+"/master.m3u8?") || strings.Contains(tsA, "tok-a") || !strings.Contains(tsA, "api_key="+proxyToken) {
		t.Fatalf("unexpected TranscodingUrl for A: %q", tsA)
	}

	// Extract proxyPlaySessionID for A from TranscodingUrl and verify active_play_sessions row.
	uA, err := url.Parse(tsA)
	if err != nil {
		t.Fatalf("parse TranscodingUrl: %v", err)
	}
	psIDA := uA.Query().Get("PlaySessionId")
	psRow, err := database.GetPlaySession(ctx, psIDA)
	if err != nil {
		t.Fatalf("GetPlaySession(%s) failed: %v", psIDA, err)
	}
	if psRow.RemotePlaySessionID != "remote-ps-a-hls" || !strings.HasPrefix(psRow.UpstreamTranscodingURL, upA.URL+"/videos/remote-a/master.m3u8") {
		t.Fatalf("unexpected ActivePlaySession row: %+v", psRow)
	}

	// 2. Request with specific MediaSourceId=srcAID -> srcAID must be selected.
	req = httptest.NewRequest(http.MethodPost, "/Items/"+itemID+"/PlaybackInfo?MediaSourceId="+srcAID, bytes.NewBufferString(`{"DeviceId":"tv-1"}`))
	req.Header.Set("X-Emby-Token", proxyToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PlaybackInfo targeted status = %d", rec.Code)
	}
	var targeted struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &targeted)
	if len(targeted.MediaSources) != 1 || targeted.MediaSources[0]["Id"] != srcAID {
		t.Fatalf("expected only %s when MediaSourceId specified, got %+v", srcAID, targeted.MediaSources)
	}

	// 3. DELETE /Videos/ActiveEncodings by PlaySessionId forwards remote-ps-a-hls and deletes row.
	delReq := httptest.NewRequest(http.MethodDelete, "/Videos/ActiveEncodings?PlaySessionId="+psIDA, nil)
	delReq.Header.Set("X-Emby-Token", proxyToken)
	delRec := httptest.NewRecorder()
	handler.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ActiveEncodings status = %d, want 204", delRec.Code)
	}
	if _, err := database.GetPlaySession(ctx, psIDA); err == nil {
		t.Fatalf("expected play session %s to be deleted", psIDA)
	}
	mu.Lock()
	if len(stopCalls) != 1 || stopCalls[0].Get("playSessionId") != "remote-ps-a-hls" {
		t.Fatalf("expected upstream StopActiveEncodings with remote-ps-a-hls, got %+v", stopCalls)
	}
	mu.Unlock()

	// 4. DELETE /Videos/ActiveEncodings by DeviceId cleans up remaining device sessions.
	delDevReq := httptest.NewRequest(http.MethodDelete, "/Videos/ActiveEncodings?DeviceId=tv-1", nil)
	delDevReq.Header.Set("X-Emby-Token", proxyToken)
	delDevRec := httptest.NewRecorder()
	handler.ServeHTTP(delDevRec, delDevReq)
	if delDevRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ActiveEncodings by DeviceId status = %d, want 204", delDevRec.Code)
	}
	var remaining int
	if err := database.SqlDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM active_play_sessions WHERE device_id = 'tv-1'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("expected 0 remaining sessions for tv-1, got %d (err=%v)", remaining, err)
	}
}

func TestPlaybackInfoPartialUpstreamFailureAndAllFail502(t *testing.T) {
	handler, database, pool, _, proxyToken := setupPlaybackTestRouter(t)
	ctx := context.Background()

	// UpGood works, UpBad returns 500.
	upGood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": "Good", "Version": "12.4.0"})
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"User": map[string]any{"Id": "ug"}, "AccessToken": "tg"})
		case "/Items/good-item/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"PlaySessionId": "ps-good",
				"MediaSources": []map[string]any{
					{"Id": "ms-good", "Container": "mp4", "SupportsDirectPlay": true},
				},
			})
		}
	}))
	defer upGood.Close()

	var badFail bool
	upBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": "Bad", "Version": "12.4.0"})
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"User": map[string]any{"Id": "ub"}, "AccessToken": "tb"})
		case "/Items/bad-item/PlaybackInfo":
			_ = badFail
			http.Error(w, "upstream down", http.StatusInternalServerError)
		}
	}))
	defer upBad.Close()

	srvGood, err := pool.AddOrUpdateServer(ctx, "", "Good", upGood.URL, "a", "p", 10, "proxy", true)
	if err != nil {
		t.Fatalf("add srvGood: %v", err)
	}
	srvBad, err := pool.AddOrUpdateServer(ctx, "", "Bad", upBad.URL, "a", "p", 20, "proxy", true)
	if err != nil {
		t.Fatalf("add srvBad: %v", err)
	}

	itemPartial := idgen.VirtualItemID("movie:partial")
	_ = database.UpsertItem(ctx, db.VirtualItem{ID: itemPartial, LibraryID: idgen.MoviesLibraryID(), ParentID: idgen.MoviesLibraryID(), ItemType: "Movie", CanonicalKey: "movie:partial", Name: "Partial"})
	_ = database.UpsertSource(ctx, db.ItemSource{SourceID: idgen.SourceID(srvGood.ID, "ms-good"), VirtualItemID: itemPartial, ServerID: srvGood.ID, RemoteItemID: "good-item", RemoteMediaSourceID: "ms-good", Label: "Good"})
	_ = database.UpsertSource(ctx, db.ItemSource{SourceID: idgen.SourceID(srvBad.ID, "ms-bad"), VirtualItemID: itemPartial, ServerID: srvBad.ID, RemoteItemID: "bad-item", RemoteMediaSourceID: "ms-bad", Label: "Bad"})

	req := httptest.NewRequest(http.MethodGet, "/Items/"+itemPartial+"/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", proxyToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial failure should succeed with healthy upstream, got %d: %s", rec.Code, rec.Body.String())
	}

	// Item only on Bad -> 502 Bad Gateway.
	itemOnlyBad := idgen.VirtualItemID("movie:onlybad")
	_ = database.UpsertItem(ctx, db.VirtualItem{ID: itemOnlyBad, LibraryID: idgen.MoviesLibraryID(), ParentID: idgen.MoviesLibraryID(), ItemType: "Movie", CanonicalKey: "movie:onlybad", Name: "OnlyBad"})
	_ = database.UpsertSource(ctx, db.ItemSource{SourceID: idgen.SourceID(srvBad.ID, "ms-onlybad"), VirtualItemID: itemOnlyBad, ServerID: srvBad.ID, RemoteItemID: "bad-item", RemoteMediaSourceID: "ms-onlybad", Label: "Bad"})

	req = httptest.NewRequest(http.MethodPost, "/Items/"+itemOnlyBad+"/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", proxyToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("all upstreams failed should return 502, got %d: %s", rec.Code, rec.Body.String())
	}
}
