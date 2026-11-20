package syncengine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

type spyBroadcaster struct {
	mu       sync.Mutex
	messages []string
}

func (b *spyBroadcaster) Broadcast(messageType string, data any) {
	b.mu.Lock()
	b.messages = append(b.messages, messageType)
	b.mu.Unlock()
}

func (b *spyBroadcaster) count(msgType string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, m := range b.messages {
		if m == msgType {
			n++
		}
	}
	return n
}

func newMockJellyfinServer(t *testing.T, serverName, remoteUserID, accessToken string, itemsFn func() []upstream.RemoteItem) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ServerName": serverName,
				"Version":    "12.4.0",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"User":        map[string]any{"Id": remoteUserID},
				"AccessToken": accessToken,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/"+remoteUserID+"/Items":
			if !strings.Contains(r.Header.Get("Authorization"), accessToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			all := itemsFn()
			start, _ := strconv.Atoi(r.URL.Query().Get("StartIndex"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("Limit"))
			if limit <= 0 {
				limit = len(all)
			}
			end := start + limit
			if start > len(all) {
				start = len(all)
			}
			if end > len(all) {
				end = len(all)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items":            all[start:end],
				"TotalRecordCount": len(all),
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/Items/") && strings.Contains(r.URL.Path, "/Images/"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("fake-jpeg-" + r.URL.Query().Get("maxWidth")))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/Users/"+remoteUserID+"/Items/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": "item-1",
				"UserData": map[string]any{
					"Played":                true,
					"PlaybackPositionTicks": 5000,
					"PlayCount":             2,
					"IsFavorite":            true,
				},
			})
		case r.URL.Path == "/Sessions/Playing" ||
			r.URL.Path == "/Sessions/Playing/Progress" ||
			r.URL.Path == "/Sessions/Playing/Stopped" ||
			strings.Contains(r.URL.Path, "/PlayedItems/") ||
			strings.Contains(r.URL.Path, "/FavoriteItems/") ||
			r.URL.Path == "/Videos/ActiveEncodings":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestEngineMultiServerSyncDeduplicationAndPruning(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:", "MergerFinTest")
	if err != nil {
		t.Fatalf("db.Open(:memory:) failed: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("database.Close() failed: %v", err)
		}
	}()

	adminUser, err := auth.EnsureAdminUser(ctx, database, "admin", "admin-pass")
	if err != nil {
		t.Fatalf("EnsureAdminUser failed: %v", err)
	}

	pool, err := upstream.NewPool(database, "test-secret", slog.Default())
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}

	var mu sync.Mutex
	itemsA := []upstream.RemoteItem{
		{
			ID:             "a-interstellar",
			Name:           "Interstellar",
			Type:           "Movie",
			Overview:       "Primary overview from Upstream A",
			ProductionYear: 2014,
			RunTimeTicks:   1000000000,
			Genres:         []string{"Sci-Fi", "Drama"},
			ProviderIDs:    map[string]string{"Tmdb": "157336", "Imdb": "tt0816692"},
			ImageTags:      map[string]string{"Primary": "tag-a-1"},
			BackdropImageTags: []string{"bd-a-1"},
			MediaSources: []upstream.RemoteMediaSource{
				{
					ID:        "a-ms-interstellar",
					Container: "mp4",
					Size:      1024000,
					Bitrate:   4200000,
					MediaStreams: []upstream.RemoteMediaStream{
						{Type: "Video", Codec: "h264", Width: 1920, Height: 1080, Index: 0},
						{Type: "Audio", Codec: "aac", Index: 1},
					},
				},
			},
			UserData: &upstream.RemoteUserData{
				PlaybackPositionTicks: 120000000,
				PlayCount:             0,
				Played:                false,
			},
		},
		{
			ID:             "a-arrival",
			Name:           "Arrival",
			Type:           "Movie",
			Overview:       "Exclusive on A",
			ProductionYear: 2016,
			ProviderIDs:    map[string]string{"Tmdb": "329865"},
			MediaSources: []upstream.RemoteMediaSource{
				{
					ID:        "a-ms-arrival",
					Container: "mkv",
					Bitrate:   5000000,
					MediaStreams: []upstream.RemoteMediaStream{
						{Type: "Video", Codec: "h264", Width: 1920, Height: 1080},
					},
				},
			},
		},
		{
			ID:          "a-severance",
			Name:        "Severance",
			Type:        "Series",
			Overview:    "Severance from A",
			ProviderIDs: map[string]string{"Tmdb": "95396", "Tvdb": "371980"},
			ImageTags:   map[string]string{"Primary": "tag-sev-a"},
		},
		{
			ID:          "a-sev-s1",
			Name:        "Season 1",
			Type:        "Season",
			SeriesID:    "a-severance",
			SeriesName:  "Severance",
			IndexNumber: 1,
		},
		{
			ID:                "a-sev-s1e1",
			Name:              "Good News About Hell",
			Type:              "Episode",
			SeriesID:          "a-severance",
			SeriesName:        "Severance",
			SeasonID:          "a-sev-s1",
			SeasonName:        "Season 1",
			ParentIndexNumber: 1,
			IndexNumber:       1,
			MediaSources: []upstream.RemoteMediaSource{
				{
					ID:        "a-ms-s1e1",
					Container: "mp4",
					Bitrate:   3000000,
					MediaStreams: []upstream.RemoteMediaStream{
						{Type: "Video", Codec: "h264", Width: 1920, Height: 1080},
					},
				},
			},
		},
	}

	itemsB := []upstream.RemoteItem{
		{
			ID:             "b-interstellar",
			Name:           "Interstellar",
			Type:           "Movie",
			Overview:       "Secondary overview from B (should NOT overwrite A)",
			Taglines:       []string{"Mankind was born on Earth. It was never meant to die here."},
			ProductionYear: 2014,
			ProviderIDs:    map[string]string{"Tmdb": "157336"},
			ImageTags:      map[string]string{"Primary": "tag-b-1"},
			MediaSources: []upstream.RemoteMediaSource{
				{
					ID:        "b-ms-interstellar",
					Container: "mkv",
					Size:      512000,
					Bitrate:   2100000,
					MediaStreams: []upstream.RemoteMediaStream{
						{Type: "Video", Codec: "hevc", Width: 1280, Height: 720, Index: 0},
						{Type: "Audio", Codec: "opus", Index: 1},
					},
				},
			},
		},
		{
			ID:          "b-severance",
			Name:        "Severance",
			Type:        "Series",
			Overview:    "Severance from B",
			ProviderIDs: map[string]string{"Tmdb": "95396"},
		},
		// Intentionally omit Season object on B to test automatic Season resolution/placeholder!
		{
			ID:                "b-sev-s1e2",
			Name:              "Half Loop",
			Type:              "Episode",
			SeriesID:          "b-severance",
			SeriesName:        "Severance",
			ParentIndexNumber: 1,
			IndexNumber:       2,
			MediaSources: []upstream.RemoteMediaSource{
				{
					ID:        "b-ms-s1e2",
					Container: "mp4",
					Bitrate:   2500000,
					MediaStreams: []upstream.RemoteMediaStream{
						{Type: "Video", Codec: "h264", Width: 1920, Height: 1080},
					},
				},
			},
		},
	}

	srvAHTTP := newMockJellyfinServer(t, "Upstream A", "user-a", "token-a", func() []upstream.RemoteItem {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstream.RemoteItem(nil), itemsA...)
	})
	defer srvAHTTP.Close()

	srvBHTTP := newMockJellyfinServer(t, "Upstream B", "user-b", "token-b", func() []upstream.RemoteItem {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstream.RemoteItem(nil), itemsB...)
	})
	defer srvBHTTP.Close()

	srvA, err := pool.AddOrUpdateServer(ctx, "", "Upstream A", srvAHTTP.URL, "admin", "pass", 10, "proxy", true)
	if err != nil {
		t.Fatalf("add server A: %v", err)
	}
	srvB, err := pool.AddOrUpdateServer(ctx, "", "Upstream B", srvBHTTP.URL, "admin", "pass", 20, "proxy", true)
	if err != nil {
		t.Fatalf("add server B: %v", err)
	}

	ws := &spyBroadcaster{}
	upClient := upstream.NewClient()
	engine := NewEngine(database, pool, upClient, ws, slog.Default())

	if err := engine.TriggerSync(ctx); err != nil {
		t.Fatalf("TriggerSync failed: %v", err)
	}

	st := engine.Status()
	if st.Running || st.LastSyncAt == nil || st.LastError != "" || st.ItemsIndexed == 0 {
		t.Fatalf("unexpected sync status: %+v", st)
	}
	if ws.count("LibraryChanged") != 1 {
		t.Fatalf("expected 1 LibraryChanged broadcast, got %d", ws.count("LibraryChanged"))
	}

	// Verify Interstellar deduplication, priority metadata, and tagline backfill from B.
	interstellarID := idgen.VirtualItemID("movie:tmdb:157336")
	interstellar, err := database.GetItemByID(ctx, interstellarID)
	if err != nil {
		t.Fatalf("GetItemByID(Interstellar) failed: %v", err)
	}
	if interstellar.Overview != "Primary overview from Upstream A" {
		t.Fatalf("expected A overview to win, got %q", interstellar.Overview)
	}
	if interstellar.Tagline != "Mankind was born on Earth. It was never meant to die here." {
		t.Fatalf("expected B tagline backfill, got %q", interstellar.Tagline)
	}
	if interstellar.ImgPrimaryServerID != srvA.ID || interstellar.ImgPrimaryRemoteID != "a-interstellar" {
		t.Fatalf("expected primary image from A, got server=%q remote=%q", interstellar.ImgPrimaryServerID, interstellar.ImgPrimaryRemoteID)
	}

	sources, err := database.ListSourcesByVirtualItemID(ctx, interstellarID)
	if err != nil {
		t.Fatalf("ListSourcesByVirtualItemID failed: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("expected 2 sources for Interstellar, got %d", len(sources))
	}
	if sources[0].Label != "[Upstream A] 1080p H264 (4.2 Mbps)" || sources[1].Label != "[Upstream B] 720p HEVC (2.1 Mbps)" {
		t.Fatalf("unexpected source labels/order: %+v", sources)
	}

	// Verify UserData merged onto local admin user.
	ud, err := database.GetUserItemData(ctx, adminUser.ID, interstellarID)
	if err != nil {
		t.Fatalf("GetUserItemData failed: %v", err)
	}
	if ud.PlaybackPositionTicks != 120000000 {
		t.Fatalf("expected PlaybackPositionTicks=120000000, got %d", ud.PlaybackPositionTicks)
	}

	// Verify cross-server TV Series / Season / Episode merging.
	seriesID := idgen.VirtualItemID("series:tmdb:95396")
	seasons, err := database.GetSeasons(ctx, seriesID)
	if err != nil || len(seasons) != 1 {
		t.Fatalf("expected 1 merged season, got %d (err=%v)", len(seasons), err)
	}
	episodes, err := database.GetEpisodes(ctx, seriesID, seasons[0].ID)
	if err != nil || len(episodes) != 2 {
		t.Fatalf("expected 2 merged episodes across A+B, got %d (err=%v)", len(episodes), err)
	}
	if episodes[0].IndexNumber != 1 || episodes[1].IndexNumber != 2 {
		t.Fatalf("unexpected episode ordering: %+v", episodes)
	}

	// Verify FTS5 search works on indexed items.
	ftsItems, ftsTotal, err := database.QueryItems(ctx, db.ItemFilter{SearchTerm: "Interstellar"})
	if err != nil || ftsTotal != 1 || len(ftsItems) != 1 {
		t.Fatalf("FTS5 search failed: total=%d len=%d err=%v", ftsTotal, len(ftsItems), err)
	}

	// Verify orphan pruning when Arrival and all Severance episodes are removed from upstreams.
	mu.Lock()
	itemsA = itemsA[:1] // keep only Interstellar on A
	itemsB = itemsB[:1] // keep only Interstellar on B
	mu.Unlock()

	if err := engine.TriggerSync(ctx); err != nil {
		t.Fatalf("second TriggerSync failed: %v", err)
	}

	if _, err := database.GetItemByID(ctx, idgen.VirtualItemID("movie:tmdb:329865")); err == nil {
		t.Fatalf("expected Arrival to be orphan-pruned")
	}
	if _, err := database.GetItemByID(ctx, seriesID); err == nil {
		t.Fatalf("expected Severance series to be orphan-pruned after all episodes disappeared")
	}

	// Interstellar should still exist with 2 sources.
	sourcesAfter, err := database.ListSourcesByVirtualItemID(ctx, interstellarID)
	if err != nil || len(sourcesAfter) != 2 {
		t.Fatalf("expected Interstellar to keep 2 sources, got %d (err=%v)", len(sourcesAfter), err)
	}

	// Exercise upstream.Client helper endpoints against srvB mock.
	decB, err := pool.GetDecryptedServer(ctx, srvB.ID)
	if err != nil {
		t.Fatalf("GetDecryptedServer failed: %v", err)
	}
	imgData, ct, status, err := upClient.FetchImage(ctx, *decB, "b-interstellar", "Primary", url.Values{"maxWidth": {"300"}})
	if err != nil || status != http.StatusOK || ct != "image/jpeg" || string(imgData) != "fake-jpeg-300" {
		t.Fatalf("FetchImage failed: status=%d ct=%q data=%q err=%v", status, ct, string(imgData), err)
	}
	pbResp, pbStatus, err := upClient.PostPlaybackInfo(ctx, *decB, "b-interstellar", url.Values{"UserId": {"user-b"}}, []byte(`{"MaxStreamingBitrate":8000000}`))
	if err != nil || pbStatus != http.StatusOK || !strings.Contains(string(pbResp), "8000000") {
		t.Fatalf("PostPlaybackInfo failed: status=%d body=%q err=%v", pbStatus, string(pbResp), err)
	}
	if err := upClient.ReportPlaying(ctx, *decB, []byte(`{}`)); err != nil {
		t.Fatalf("ReportPlaying failed: %v", err)
	}
	if err := upClient.ReportProgress(ctx, *decB, []byte(`{}`)); err != nil {
		t.Fatalf("ReportProgress failed: %v", err)
	}
	if err := upClient.ReportStopped(ctx, *decB, []byte(`{}`)); err != nil {
		t.Fatalf("ReportStopped failed: %v", err)	}
	if err := upClient.MarkPlayed(ctx, *decB, "b-interstellar", true); err != nil {
		t.Fatalf("MarkPlayed failed: %v", err)
	}
	if err := upClient.MarkFavorite(ctx, *decB, "b-interstellar", true); err != nil {
		t.Fatalf("MarkFavorite failed: %v", err)
	}
	if err := upClient.StopActiveEncodings(ctx, *decB, "dev-1", "ps-1"); err != nil {
		t.Fatalf("StopActiveEncodings failed: %v", err)
	}
	remoteUD, err := upClient.GetUserItemData(ctx, *decB, "b-interstellar")
	if err != nil || !remoteUD.Played || !remoteUD.IsFavorite || remoteUD.PlayCount != 2 {
		t.Fatalf("GetUserItemData failed: ud=%+v err=%v", remoteUD, err)
	}
}
