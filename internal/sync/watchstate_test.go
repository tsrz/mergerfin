package syncengine

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

func setupWatchStateTestEnv(t *testing.T) (*WatchStateManager, *db.DB, *upstream.Pool, *spyBroadcaster, string, string, string, string, *[]string, *[]string, *sync.Mutex) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:", "WatchTest")
	if err != nil {
		t.Fatalf("db.Open(:memory:) failed: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	user, err := auth.EnsureAdminUser(ctx, database, "admin", "pass")
	if err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}

	pool, err := upstream.NewPool(database, "secret", slog.Default())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	var mu sync.Mutex
	var eventsA []string
	var eventsB []string

	makeServer := func(name, uid, tok string, events *[]string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public":
				_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": name, "Version": "12.4.0"})
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"User": map[string]any{"Id": uid}, "AccessToken": tok})
			default:
				mu.Lock()
				*events = append(*events, r.Method+" "+r.URL.Path)
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}
		}))
	}

	upA := makeServer("UpA", "ua", "tok-a", &eventsA)
	t.Cleanup(upA.Close)
	upB := makeServer("UpB", "ub", "tok-b", &eventsB)
	t.Cleanup(upB.Close)

	srvA, err := pool.AddOrUpdateServer(ctx, "", "UpA", upA.URL, "a", "p", 10, "proxy", true)
	if err != nil {
		t.Fatalf("add srvA: %v", err)
	}
	srvB, err := pool.AddOrUpdateServer(ctx, "", "UpB", upB.URL, "a", "p", 20, "proxy", true)
	if err != nil {
		t.Fatalf("add srvB: %v", err)
	}

	movieID := idgen.VirtualItemID("movie:tmdb:157336")
	if err := database.UpsertItem(ctx, db.VirtualItem{
		ID:           movieID,
		LibraryID:    idgen.MoviesLibraryID(),
		ParentID:     idgen.MoviesLibraryID(),
		ItemType:     "Movie",
		CanonicalKey: "movie:tmdb:157336",
		Name:         "Interstellar",
		RunTimeTicks: 1000,
	}); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	srcAID := idgen.SourceID(srvA.ID, "ms-a")
	srcBID := idgen.SourceID(srvB.ID, "ms-b")
	_ = database.UpsertSource(ctx, db.ItemSource{SourceID: srcAID, VirtualItemID: movieID, ServerID: srvA.ID, RemoteItemID: "rem-a", RemoteMediaSourceID: "ms-a", Label: "A"})
	_ = database.UpsertSource(ctx, db.ItemSource{SourceID: srcBID, VirtualItemID: movieID, ServerID: srvB.ID, RemoteItemID: "rem-b", RemoteMediaSourceID: "ms-b", Label: "B"})

	psID := idgen.NewRandomHex32()
	_ = database.CreatePlaySession(ctx, db.ActivePlaySession{
		PlaySessionID:       psID,
		UserID:              user.ID,
		DeviceID:            "dev-1",
		VirtualItemID:       movieID,
		SourceID:            srcAID,
		ServerID:            srvA.ID,
		RemoteItemID:        "rem-a",
		RemotePlaySessionID: "rps-a",
	})

	ws := &spyBroadcaster{}
	mgr := NewWatchStateManager(database, pool, upstream.NewClient(), ws, slog.Default())
	return mgr, database, pool, ws, user.ID, movieID, psID, srvA.ID, &eventsA, &eventsB, &mu
}

func TestWatchStateProgressThrottleAndPreservePlayed(t *testing.T) {
	mgr, database, _, _, userID, movieID, psID, _, eventsA, _, mu := setupWatchStateTestEnv(t)
	ctx := context.Background()

	// Pre-mark played to confirm OnPlaying/OnProgress preserve existing played flag.
	if err := database.SetPlayed(ctx, userID, movieID, true, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SetPlayed: %v", err)
	}

	fakeNow := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	mgr.nowFn = func() time.Time { return fakeNow }

	if err := mgr.OnPlaying(ctx, userID, movieID, psID, 100); err != nil {
		t.Fatalf("OnPlaying: %v", err)
	}
	ud, _ := database.GetUserItemData(ctx, userID, movieID)
	if !ud.Played || ud.PlaybackPositionTicks != 100 {
		t.Fatalf("OnPlaying should preserve played=true and set pos=100, got %+v", ud)
	}

	// First OnProgress relays immediately.
	if err := mgr.OnProgress(ctx, userID, movieID, psID, 200, false); err != nil {
		t.Fatalf("OnProgress 1: %v", err)
	}
	// Within 10s with same pause state: SQLite updates, upstream throttled.
	fakeNow = fakeNow.Add(4 * time.Second)
	if err := mgr.OnProgress(ctx, userID, movieID, psID, 300, false); err != nil {
		t.Fatalf("OnProgress 2: %v", err)
	}
	ud, _ = database.GetUserItemData(ctx, userID, movieID)
	if !ud.Played || ud.PlaybackPositionTicks != 300 {
		t.Fatalf("expected pos=300 and played=true, got %+v", ud)
	}

	mu.Lock()
	countBeforePause := len(*eventsA)
	mu.Unlock()
	if countBeforePause != 2 { // Playing + 1st Progress
		t.Fatalf("expected 2 upstream events (Playing + 1st Progress), got %d: %+v", countBeforePause, *eventsA)
	}

	// Pause state transition bypasses 10s throttle.
	fakeNow = fakeNow.Add(1 * time.Second)
	if err := mgr.OnProgress(ctx, userID, movieID, psID, 350, true); err != nil {
		t.Fatalf("OnProgress pause: %v", err)
	}
	mu.Lock()
	countAfterPause := len(*eventsA)
	mu.Unlock()
	if countAfterPause != 3 {
		t.Fatalf("expected pause transition to relay immediately (3 total), got %d", countAfterPause)
	}

	// After 10s elapsed, next progress relays.
	fakeNow = fakeNow.Add(11 * time.Second)
	if err := mgr.OnProgress(ctx, userID, movieID, psID, 400, true); err != nil {
		t.Fatalf("OnProgress after 10s: %v", err)
	}
	mu.Lock()
	countAfter10s := len(*eventsA)
	mu.Unlock()
	if countAfter10s != 4 {
		t.Fatalf("expected 4 upstream events after 10s elapsed, got %d", countAfter10s)
	}
}

func TestWatchStateOnStoppedAutoPlayedAndMultiServerFanOut(t *testing.T) {
	mgr, database, _, ws, userID, movieID, psID, _, eventsA, eventsB, mu := setupWatchStateTestEnv(t)
	ctx := context.Background()

	// Stop at 900/1000 ticks (90%) -> auto-marked played, stops encoding, deletes session, fans out to A & B.
	if err := mgr.OnStopped(ctx, userID, movieID, psID, 900); err != nil {
		t.Fatalf("OnStopped: %v", err)
	}
	mgr.Flush(5 * time.Second)

	ud, err := database.GetUserItemData(ctx, userID, movieID)
	if err != nil || !ud.Played {
		t.Fatalf("expected Played=true at 90%% runtime, got %+v (err=%v)", ud, err)
	}
	if _, err := database.GetPlaySession(ctx, psID); err == nil {
		t.Fatalf("expected play session %s to be deleted on stop", psID)
	}
	if ws.count("UserDataChanged") != 1 {
		t.Fatalf("expected UserDataChanged broadcast on stop, got %d", ws.count("UserDataChanged"))
	}

	mu.Lock()
	evA := append([]string(nil), (*eventsA)...)
	evB := append([]string(nil), (*eventsB)...)
	mu.Unlock()

	if len(evA) < 3 {
		t.Fatalf("expected StopActiveEncodings, ReportStopped, and MarkPlayed on A, got %+v", evA)
	}
	if len(evB) != 1 || evB[0] != "POST /Users/ub/PlayedItems/rem-b" {
		t.Fatalf("expected MarkPlayed fan-out on B, got %+v", evB)
	}

	// Verify SetPlayed(false) and SetFavorite(true) fan out to both A and B.
	ud, err = mgr.SetPlayed(ctx, userID, movieID, false)
	if err != nil || ud.Played {
		t.Fatalf("SetPlayed(false) failed: %+v err=%v", ud, err)
	}
	ud, err = mgr.SetFavorite(ctx, userID, movieID, true)
	if err != nil || !ud.IsFavorite {
		t.Fatalf("SetFavorite(true) failed: %+v err=%v", ud, err)
	}
	mgr.Flush(5 * time.Second)

	if ws.count("UserDataChanged") != 3 {
		t.Fatalf("expected 3 UserDataChanged broadcasts total, got %d", ws.count("UserDataChanged"))
	}
}
