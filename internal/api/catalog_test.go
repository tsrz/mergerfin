package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tsrz/mergerfin/internal/admin"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/config"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

func insertHyphenatedUUID(hex32 string) string {
	return hex32[0:8] + "-" + hex32[8:12] + "-" + hex32[12:16] + "-" + hex32[16:20] + "-" + hex32[20:32]
}

type catalogFixture struct {
	handler    http.Handler
	database   *db.DB
	pool       *upstream.Pool
	cfg        *config.Config
	token      string
	userID     string
	movieID    string
	seriesID   string
	season1ID  string
	ep1ID      string
	ep2ID      string
	server1ID  string
	imageHits  *atomic.Int32
	mockUpSrv  *httptest.Server
}

func setupCatalogFixture(t *testing.T) *catalogFixture {
	t.Helper()
	ctx := context.Background()

	tmpDir := t.TempDir()
	cfg := &config.Config{
		Port:       "8096",
		DataDir:    tmpDir,
		ServerName: "MergerFin-Catalog-Test",
		SecretKey:  "catalog-secret-key",
	}

	database, err := db.Open(ctx, filepath.Join(tmpDir, "test.db"), cfg.ServerName)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	user, err := auth.EnsureAdminUser(ctx, database, "admin", "adminpass")
	if err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}
	token := idgen.NewRandomHex32()
	if err := database.UpsertSession(ctx, db.ProxySession{
		Token:      token,
		UserID:     user.ID,
		DeviceID:   idgen.NewRandomHex32(),
		ClientName: "TestClient",
		DeviceName: "TestDevice",
		AppVersion: "12.4.0",
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	pool, err := upstream.NewPool(database, cfg.SecretKey, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	var imageHits atomic.Int32
	mockUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/Items/") && strings.Contains(r.URL.Path, "/Images/") {
			imageHits.Add(1)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nmockpngdata"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(mockUp.Close)

	srv1ID := idgen.NewRandomHex32()
	srv2ID := idgen.NewRandomHex32()
	encTok1, _ := pool.EncryptToken("up-tok-1")
	encTok2, _ := pool.EncryptToken("up-tok-2")

	for _, srv := range []db.UpstreamServer{
		{ID: srv1ID, Name: "Upstream A", BaseURL: mockUp.URL, Username: "u1", RemoteUserID: "ru1", AccessToken: encTok1, Priority: 10, StreamMode: "proxy", Enabled: true, IsOnline: true},
		{ID: srv2ID, Name: "Upstream B", BaseURL: mockUp.URL, Username: "u2", RemoteUserID: "ru2", AccessToken: encTok2, Priority: 20, StreamMode: "redirect", Enabled: true, IsOnline: true},
	} {
		if err := database.UpsertServer(ctx, srv); err != nil {
			t.Fatalf("UpsertServer: %v", err)
		}
	}

	moviesLib := idgen.MoviesLibraryID()
	tvLib := idgen.TVShowsLibraryID()

	movieKey := "movie:tmdb:603"
	movieID := idgen.VirtualItemID(movieKey)
	if err := database.UpsertItem(ctx, db.VirtualItem{
		ID:                  movieID,
		LibraryID:           moviesLib,
		ParentID:            moviesLib,
		ItemType:            "Movie",
		CanonicalKey:        movieKey,
		Name:                "The Matrix",
		OriginalTitle:       "The Matrix",
		SortName:            "Matrix, The",
		Overview:            "A computer hacker learns about the true nature of reality.",
		Tagline:             "Welcome to the Real World.",
		ProductionYear:      1999,
		PremiereDate:        "1999-03-31T00:00:00Z",
		CommunityRating:     8.7,
		OfficialRating:      "R",
		RunTimeTicks:        81600000000,
		GenresJSON:          `["Action","Sci-Fi"]`,
		StudiosJSON:         `[{"Name":"Warner Bros."}]`,
		PeopleJSON:          `[{"Name":"Keanu Reeves","Role":"Neo","Type":"Actor"}]`,
		ProviderIDsJSON:     `{"Tmdb":"603","Imdb":"tt0133093"}`,
		ImgPrimaryServerID:  srv1ID,
		ImgPrimaryRemoteID:  "remote-matrix-1",
		ImgBackdropServerID: srv1ID,
		ImgBackdropRemoteID: "remote-matrix-1",
		DateCreated:         "2026-01-02T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertItem movie: %v", err)
	}

	// Second movie (older, different genre) for Similar + Filters tests
	movie2ID := idgen.VirtualItemID("movie:tmdb:604")
	if err := database.UpsertItem(ctx, db.VirtualItem{
		ID:              movie2ID,
		LibraryID:       moviesLib,
		ParentID:        moviesLib,
		ItemType:        "Movie",
		CanonicalKey:    "movie:tmdb:604",
		Name:            "The Matrix Reloaded",
		SortName:        "Matrix Reloaded, The",
		Overview:        "Neo and his allies race against time.",
		ProductionYear:  2003,
		CommunityRating: 7.2,
		OfficialRating:  "R",
		GenresJSON:      `["Action","Sci-Fi"]`,
		ProviderIDsJSON: `{"Tmdb":"604"}`,
		DateCreated:     "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertItem movie2: %v", err)
	}

	// 2 MediaSources for The Matrix (from Upstream A and Upstream B)
	for _, src := range []db.ItemSource{
		{
			SourceID:            idgen.SourceID(srv1ID, "ms-a"),
			VirtualItemID:       movieID,
			ServerID:            srv1ID,
			RemoteItemID:        "remote-matrix-1",
			RemoteMediaSourceID: "ms-a",
			Label:               "[Upstream A] 2160p HEVC",
			Container:           "mkv",
			VideoCodec:          "hevc",
			AudioCodec:          "eac3",
			Width:               3840,
			Height:              2160,
			Bitrate:             25000000,
			SizeBytes:           24000000000,
			RunTimeTicks:        81600000000,
			RawMediaStreamsJSON: `[{"Type":"Video","Codec":"hevc","Index":0,"Width":3840,"Height":2160}]`,
			RawMediaSourceJSON:  `{"DefaultAudioStreamIndex":1}`,
		},
		{
			SourceID:            idgen.SourceID(srv2ID, "ms-b"),
			VirtualItemID:       movieID,
			ServerID:            srv2ID,
			RemoteItemID:        "remote-matrix-2",
			RemoteMediaSourceID: "ms-b",
			Label:               "[Upstream B] 1080p H264",
			Container:           "mp4",
			VideoCodec:          "h264",
			AudioCodec:          "aac",
			Width:               1920,
			Height:              1080,
			Bitrate:             8000000,
			SizeBytes:           8000000000,
			RunTimeTicks:        81600000000,
			RawMediaStreamsJSON: `[{"Type":"Video","Codec":"h264","Index":0,"Width":1920,"Height":1080}]`,
			RawMediaSourceJSON:  `{}`,
		},
		{
			SourceID:            idgen.SourceID(srv1ID, "ms-c"),
			VirtualItemID:       movie2ID,
			ServerID:            srv1ID,
			RemoteItemID:        "remote-matrix2",
			RemoteMediaSourceID: "ms-c",
			Label:               "[Upstream A] 1080p H264",
			Container:           "mkv",
		},
	} {
		if err := database.UpsertSource(ctx, src); err != nil {
			t.Fatalf("UpsertSource: %v", err)
		}
	}

	// Series + Season 1 + Episode 1 (watched) + Episode 2 (unwatched -> NextUp!)
	seriesKey := "series:tmdb:1396"
	seriesID := idgen.VirtualItemID(seriesKey)
	season1Key := "season:" + seriesKey + ":s1"
	season1ID := idgen.VirtualItemID(season1Key)
	ep1Key := "episode:" + seriesKey + ":s1:e1"
	ep1ID := idgen.VirtualItemID(ep1Key)
	ep2Key := "episode:" + seriesKey + ":s1:e2"
	ep2ID := idgen.VirtualItemID(ep2Key)

	for _, it := range []db.VirtualItem{
		{
			ID:           seriesID,
			LibraryID:    tvLib,
			ParentID:     tvLib,
			ItemType:     "Series",
			CanonicalKey: seriesKey,
			Name:         "Breaking Bad",
			SortName:     "Breaking Bad",
			GenresJSON:   `["Drama","Crime"]`,
			DateCreated:  "2026-01-03T00:00:00Z",
		},
		{
			ID:           season1ID,
			LibraryID:    tvLib,
			ParentID:     seriesID,
			SeriesID:     seriesID,
			ItemType:     "Season",
			CanonicalKey: season1Key,
			Name:         "Season 1",
			SeriesName:   "Breaking Bad",
			IndexNumber:  1,
			DateCreated:  "2026-01-03T00:00:00Z",
		},
		{
			ID:                ep1ID,
			LibraryID:         tvLib,
			ParentID:          season1ID,
			SeriesID:          seriesID,
			SeasonID:          season1ID,
			ItemType:          "Episode",
			CanonicalKey:      ep1Key,
			Name:              "Pilot",
			SeriesName:        "Breaking Bad",
			SeasonName:        "Season 1",
			IndexNumber:       1,
			ParentIndexNumber: 1,
			DateCreated:       "2026-01-03T00:00:00Z",
		},
		{
			ID:                ep2ID,
			LibraryID:         tvLib,
			ParentID:          season1ID,
			SeriesID:          seriesID,
			SeasonID:          season1ID,
			ItemType:          "Episode",
			CanonicalKey:      ep2Key,
			Name:              "Cat's in the Bag...",
			SeriesName:        "Breaking Bad",
			SeasonName:        "Season 1",
			IndexNumber:       2,
			ParentIndexNumber: 1,
			DateCreated:       "2026-01-04T00:00:00Z",
		},
	} {
		if err := database.UpsertItem(ctx, it); err != nil {
			t.Fatalf("UpsertItem TV: %v", err)
		}
	}

	for _, src := range []db.ItemSource{
		{SourceID: idgen.SourceID(srv1ID, "ep1"), VirtualItemID: ep1ID, ServerID: srv1ID, RemoteItemID: "r-ep1", RemoteMediaSourceID: "ep1", Label: "[Upstream A] 1080p"},
		{SourceID: idgen.SourceID(srv1ID, "ep2"), VirtualItemID: ep2ID, ServerID: srv1ID, RemoteItemID: "r-ep2", RemoteMediaSourceID: "ep2", Label: "[Upstream A] 1080p"},
	} {
		if err := database.UpsertSource(ctx, src); err != nil {
			t.Fatalf("UpsertSource ep: %v", err)
		}
	}

	// Mark Ep 1 watched and Movie 2 in progress (Resume)
	if err := database.SetPlayed(ctx, user.ID, ep1ID, true, "2026-01-05T10:00:00Z"); err != nil {
		t.Fatalf("SetPlayed: %v", err)
	}
	if err := database.UpsertProgress(ctx, user.ID, movie2ID, 123450000, false, "2026-01-06T12:00:00Z"); err != nil {
		t.Fatalf("UpsertProgress: %v", err)
	}

	wsHub := NewWSHub(database, nil)
	t.Cleanup(wsHub.CloseAll)
	adm := admin.NewHandler(database, pool, nil, nil)
	router := NewRouter(RouterDeps{
		Config: cfg,
		DB:     database,
		Pool:   pool,
		Client: upstream.NewClient(),
		Admin:  adm,
		WSHub:  wsHub,
	})

	return &catalogFixture{
		handler:   router,
		database:  database,
		pool:      pool,
		cfg:       cfg,
		token:     token,
		userID:    user.ID,
		movieID:   movieID,
		seriesID:  seriesID,
		season1ID: season1ID,
		ep1ID:     ep1ID,
		ep2ID:     ep2ID,
		server1ID: srv1ID,
		imageHits: &imageHits,
		mockUpSrv: mockUp,
	}
}

func (f *catalogFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Emby-Token", f.token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestCatalogUserViewsAndDTONonNull(t *testing.T) {
	f := setupCatalogFixture(t)

	rec := f.get(t, "/UserViews")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /UserViews: %d %s", rec.Code, rec.Body.String())
	}

	var res QueryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode /UserViews: %v", err)
	}
	if res.TotalRecordCount != 2 || len(res.Items) != 2 {
		t.Fatalf("expected 2 libraries, got %+v", res)
	}

	// Verify non-null slices/maps in raw JSON and 32-hex IDs without dashes.
	var rawMap map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rawMap)
	itemsRaw := rawMap["Items"].([]any)
	first := itemsRaw[0].(map[string]any)
	for _, nonNullKey := range []string{"ImageTags", "BackdropImageTags", "ProviderIds", "Genres", "GenreItems", "Studios", "People", "RemoteTrailers", "LockedFields", "Tags", "ExternalUrls", "Taglines"} {
		if val, exists := first[nonNullKey]; !exists || val == nil {
			t.Fatalf("field %s must be present and non-null in BaseItemDto", nonNullKey)
		}
	}
	for _, it := range res.Items {
		if strings.Contains(it.Id, "-") || len(it.Id) != 32 {
			t.Fatalf("expected 32-hex ID without dashes, got %q", it.Id)
		}
		if it.Type != "CollectionFolder" || !it.IsFolder || it.LocationType != "FileSystem" {
			t.Fatalf("unexpected library DTO: %+v", it)
		}
	}
}

func TestCatalogItemsFilteringSearchAndMultipleSources(t *testing.T) {
	f := setupCatalogFixture(t)

	// 1. Recursive Movie query under Movies library
	rec := f.get(t, "/Items?ParentId="+insertHyphenatedUUID(idgen.MoviesLibraryID())+"&Recursive=true&IncludeItemTypes=Movie")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /Items: %d %s", rec.Code, rec.Body.String())
	}
	var res QueryResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.TotalRecordCount != 2 || len(res.Items) != 2 {
		t.Fatalf("expected 2 movies, got total=%d len=%d", res.TotalRecordCount, len(res.Items))
	}

	// 2. FTS5 search
	searchRec := f.get(t, "/Items?SearchTerm=hacker&IncludeItemTypes=Movie")
	if searchRec.Code != http.StatusOK {
		t.Fatalf("GET /Items?SearchTerm=hacker: %d %s", searchRec.Code, searchRec.Body.String())
	}
	var searchRes QueryResult
	_ = json.Unmarshal(searchRec.Body.Bytes(), &searchRes)
	if searchRes.TotalRecordCount != 1 || searchRes.Items[0].Id != f.movieID {
		t.Fatalf("expected The Matrix from FTS search, got %+v", searchRes)
	}

	// 3. ?Ids= with dashed UUID normalization
	idsRec := f.get(t, "/Items?Ids="+insertHyphenatedUUID(f.movieID))
	var idsRes QueryResult
	_ = json.Unmarshal(idsRec.Body.Bytes(), &idsRes)
	if idsRes.TotalRecordCount != 1 || idsRes.Items[0].Id != f.movieID {
		t.Fatalf("expected 1 item for ?Ids=, got %+v", idsRes)
	}

	// 4. GET /Items/{id} with 2 MediaSources
	itemRec := f.get(t, "/Users/"+insertHyphenatedUUID(f.userID)+"/Items/"+insertHyphenatedUUID(f.movieID))
	if itemRec.Code != http.StatusOK {
		t.Fatalf("GET /Items/{id}: %d %s", itemRec.Code, itemRec.Body.String())
	}
	var item BaseItemDto
	if err := json.Unmarshal(itemRec.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode item: %v", err)
	}
	if len(item.MediaSources) != 2 {
		t.Fatalf("expected 2 MediaSources, got %d", len(item.MediaSources))
	}
	if item.MediaSources[0].Name != "[Upstream A] 2160p HEVC" || item.MediaSources[1].Name != "[Upstream B] 1080p H264" {
		t.Fatalf("unexpected MediaSources ordering/labels: %+v", item.MediaSources)
	}
	for _, ms := range item.MediaSources {
		if len(ms.Id) != 32 || strings.Contains(ms.Id, "-") {
			t.Fatalf("MediaSource Id must be 32-hex, got %q", ms.Id)
		}
		if !ms.SupportsDirectPlay || !ms.SupportsDirectStream || !ms.SupportsTranscoding {
			t.Fatalf("MediaSource capability flags should be true: %+v", ms)
		}
		if len(ms.MediaStreams) != 1 {
			t.Fatalf("expected 1 MediaStream parsed, got %d", len(ms.MediaStreams))
		}
	}
}

func TestCatalogLatestResumeFiltersSimilar(t *testing.T) {
	f := setupCatalogFixture(t)

	// /Items/Latest must return a raw JSON array (not QueryResult).
	latestRec := f.get(t, "/Items/Latest?IncludeItemTypes=Movie&Limit=5")
	if latestRec.Code != http.StatusOK {
		t.Fatalf("GET /Items/Latest: %d %s", latestRec.Code, latestRec.Body.String())
	}
	trimmed := strings.TrimSpace(latestRec.Body.String())
	if !strings.HasPrefix(trimmed, "[") {
		t.Fatalf("/Items/Latest must return a raw JSON array, got: %s", trimmed)
	}
	var latest []BaseItemDto
	if err := json.Unmarshal(latestRec.Body.Bytes(), &latest); err != nil {
		t.Fatalf("decode /Items/Latest array: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("expected 2 latest movies, got %d", len(latest))
	}

	// /Items/Resume
	resumeRec := f.get(t, "/Items/Resume")
	if resumeRec.Code != http.StatusOK {
		t.Fatalf("GET /Items/Resume: %d %s", resumeRec.Code, resumeRec.Body.String())
	}
	var resumeRes QueryResult
	_ = json.Unmarshal(resumeRec.Body.Bytes(), &resumeRes)
	if resumeRes.TotalRecordCount != 1 || resumeRes.Items[0].Name != "The Matrix Reloaded" {
		t.Fatalf("unexpected /Items/Resume: %+v", resumeRes)
	}

	// /Items/Filters & /Items/Filters2
	filtRec := f.get(t, "/Items/Filters?IncludeItemTypes=Movie")
	if filtRec.Code != http.StatusOK || !strings.Contains(filtRec.Body.String(), "Sci-Fi") {
		t.Fatalf("GET /Items/Filters: %d %s", filtRec.Code, filtRec.Body.String())
	}
	filt2Rec := f.get(t, "/Items/Filters2?IncludeItemTypes=Movie")
	if filt2Rec.Code != http.StatusOK || !strings.Contains(filt2Rec.Body.String(), "Sci-Fi") {
		t.Fatalf("GET /Items/Filters2: %d %s", filt2Rec.Code, filt2Rec.Body.String())
	}

	// /Items/{id}/Similar
	simRec := f.get(t, "/Items/"+f.movieID+"/Similar")
	if simRec.Code != http.StatusOK {
		t.Fatalf("GET /Items/{id}/Similar: %d %s", simRec.Code, simRec.Body.String())
	}
	var simRes QueryResult
	_ = json.Unmarshal(simRec.Body.Bytes(), &simRes)
	if simRes.TotalRecordCount != 1 || simRes.Items[0].Name != "The Matrix Reloaded" {
		t.Fatalf("unexpected /Similar: %+v", simRes)
	}
}

func TestCatalogShowsSeasonsEpisodesNextUp(t *testing.T) {
	f := setupCatalogFixture(t)

	// Seasons
	seasonsRec := f.get(t, "/Shows/"+insertHyphenatedUUID(f.seriesID)+"/Seasons")
	if seasonsRec.Code != http.StatusOK {
		t.Fatalf("GET /Shows/{id}/Seasons: %d %s", seasonsRec.Code, seasonsRec.Body.String())
	}
	var seasonsRes QueryResult
	_ = json.Unmarshal(seasonsRec.Body.Bytes(), &seasonsRes)
	if seasonsRes.TotalRecordCount != 1 || seasonsRes.Items[0].Id != f.season1ID {
		t.Fatalf("unexpected seasons: %+v", seasonsRes)
	}
	if seasonsRes.Items[0].UserData.UnplayedItemCount == nil || *seasonsRes.Items[0].UserData.UnplayedItemCount != 1 {
		t.Fatalf("expected UnplayedItemCount=1 on season, got %+v", seasonsRes.Items[0].UserData.UnplayedItemCount)
	}

	// Episodes with ?Season=1
	epRec := f.get(t, "/Shows/"+f.seriesID+"/Episodes?Season=1")
	if epRec.Code != http.StatusOK {
		t.Fatalf("GET /Shows/{id}/Episodes: %d %s", epRec.Code, epRec.Body.String())
	}
	var epRes QueryResult
	_ = json.Unmarshal(epRec.Body.Bytes(), &epRes)
	if epRes.TotalRecordCount != 2 || len(epRes.Items[0].MediaSources) != 1 {
		t.Fatalf("expected 2 episodes with MediaSources, got %+v", epRes)
	}

	// NextUp (Pilot is watched, so Cat's in the Bag... should be NextUp)
	nextRec := f.get(t, "/Shows/NextUp")
	if nextRec.Code != http.StatusOK {
		t.Fatalf("GET /Shows/NextUp: %d %s", nextRec.Code, nextRec.Body.String())
	}
	var nextRes QueryResult
	_ = json.Unmarshal(nextRec.Body.Bytes(), &nextRes)
	if nextRes.TotalRecordCount != 1 || nextRes.Items[0].Id != f.ep2ID {
		t.Fatalf("expected episode 2 in NextUp, got %+v", nextRes)
	}
}

func TestCatalogImageDiskCacheHitAndMiss(t *testing.T) {
	f := setupCatalogFixture(t)

	imgURL := "/Items/" + insertHyphenatedUUID(f.movieID) + "/Images/Primary?maxWidth=400"

	// 1. Cache miss: fetches from mock upstream and writes to disk cache.
	req1 := httptest.NewRequest(http.MethodGet, imgURL, nil)
	rec1 := httptest.NewRecorder()
	f.handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("image miss: got %d %s", rec1.Code, rec1.Body.String())
	}
	if ct := rec1.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("expected Content-Type image/png, got %q", ct)
	}
	if !strings.HasPrefix(rec1.Body.String(), "\x89PNG") {
		t.Fatalf("unexpected image body: %q", rec1.Body.String())
	}
	if f.imageHits.Load() != 1 {
		t.Fatalf("expected 1 upstream image hit, got %d", f.imageHits.Load())
	}

	// Verify cache files exist on disk.
	entries, err := os.ReadDir(f.cfg.ImageCacheDir())
	if err != nil || len(entries) < 2 {
		t.Fatalf("expected cached files in %s, err=%v len=%d", f.cfg.ImageCacheDir(), err, len(entries))
	}

	// Close mock upstream completely to prove cache hit serves from disk!
	f.mockUpSrv.Close()

	// 2. Cache hit (GET)
	req2 := httptest.NewRequest(http.MethodGet, imgURL, nil)
	rec2 := httptest.NewRecorder()
	f.handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || rec2.Body.String() != rec1.Body.String() {
		t.Fatalf("image cache hit failed: %d", rec2.Code)
	}
	if f.imageHits.Load() != 1 {
		t.Fatalf("upstream should not be hit on cache hit, hits=%d", f.imageHits.Load())
	}

	// 3. HEAD request on cached image
	headReq := httptest.NewRequest(http.MethodHead, imgURL, nil)
	headRec := httptest.NewRecorder()
	f.handler.ServeHTTP(headRec, headReq)
	if headRec.Code != http.StatusOK || headRec.Body.Len() != 0 || headRec.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD image failed: code=%d bodyLen=%d cl=%q", headRec.Code, headRec.Body.Len(), headRec.Header().Get("Content-Length"))
	}

	// 4. Conditional ETag -> 304 Not Modified
	etag := rec2.Header().Get("ETag")
	req304 := httptest.NewRequest(http.MethodGet, imgURL, nil)
	req304.Header.Set("If-None-Match", etag)
	rec304 := httptest.NewRecorder()
	f.handler.ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified for matching ETag %q, got %d", etag, rec304.Code)
	}
}
