package stream

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

type streamTestEnv struct {
	database *db.DB
	pool     *upstream.Pool
	streamer *Streamer
	router   chi.Router
	userID   string
	token    string
}

func setupStreamEnv(t *testing.T) *streamTestEnv {
	t.Helper()
	ctx := context.Background()

	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "stream.db"), "MergerFin-Stream-Test")
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
		DeviceID:   "dev-stream-1",
		ClientName: "TestPlayer",
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	pool, err := upstream.NewPool(database, "stream-test-secret", nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	streamer := NewStreamer(database, pool, nil)
	r := chi.NewRouter()
	r.Group(func(ar chi.Router) {
		ar.Use(auth.Authenticate(database))
		ar.Get("/Videos/{itemId}/stream", streamer.HandleDirectStream)
		ar.Head("/Videos/{itemId}/stream", streamer.HandleDirectStream)
		ar.Get("/Videos/{itemId}/stream.{ext}", streamer.HandleDirectStream)
		ar.Head("/Videos/{itemId}/stream.{ext}", streamer.HandleDirectStream)
		ar.Get("/Videos/{itemId}/master.m3u8", streamer.HandleHLSPlaylist)
		ar.Get("/Videos/{itemId}/main.m3u8", streamer.HandleHLSPlaylist)
		ar.Get("/Videos/{itemId}/live.m3u8", streamer.HandleHLSPlaylist)
		ar.Get("/Videos/{itemId}/hls-proxy/*", streamer.HandleHLSProxy)
	})

	return &streamTestEnv{
		database: database,
		pool:     pool,
		streamer: streamer,
		router:   r,
		userID:   user.ID,
		token:    token,
	}
}

func TestHandleDirectStreamProxyRangeHeadAndRedirect(t *testing.T) {
	env := setupStreamEnv(t)
	ctx := context.Background()

	payload := bytes.Repeat([]byte("ABCDEFGH"), 512) // 4096 bytes
	const upToken = "upstream-direct-secret"

	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ApiKey") != upToken || r.Header.Get("X-Emby-Token") != upToken {
			http.Error(w, "unauthorized upstream", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/Videos/rem-movie-1/stream" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")

		rng := r.Header.Get("Range")
		if rng == "bytes=0-1023" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-1023/%d", len(payload)))
			w.Header().Set("Content-Length", "1024")
			w.WriteHeader(http.StatusPartialContent)
			if r.Method != http.MethodHead {
				_, _ = w.Write(payload[:1024])
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(payload)
		}
	}))
	defer upSrv.Close()

	srvProxyID := idgen.NewRandomHex32()
	srvRedirectID := idgen.NewRandomHex32()
	encTok, _ := env.pool.EncryptToken(upToken)

	if err := env.database.UpsertServer(ctx, db.UpstreamServer{
		ID: srvProxyID, Name: "ProxyUp", BaseURL: upSrv.URL, Username: "u", RemoteUserID: "ru",
		AccessToken: encTok, Priority: 10, StreamMode: "proxy", Enabled: true, IsOnline: true,
	}); err != nil {
		t.Fatalf("UpsertServer proxy: %v", err)
	}
	if err := env.database.UpsertServer(ctx, db.UpstreamServer{
		ID: srvRedirectID, Name: "RedirectUp", BaseURL: upSrv.URL, Username: "u", RemoteUserID: "ru",
		AccessToken: encTok, Priority: 20, StreamMode: "redirect", Enabled: true, IsOnline: true,
	}); err != nil {
		t.Fatalf("UpsertServer redirect: %v", err)
	}

	itemID := idgen.VirtualItemID("movie:tmdb:100")
	if err := env.database.UpsertItem(ctx, db.VirtualItem{
		ID: itemID, LibraryID: idgen.MoviesLibraryID(), ItemType: "Movie", CanonicalKey: "movie:tmdb:100", Name: "Test Movie",
	}); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	msProxyID := idgen.SourceID(srvProxyID, "ms-1")
	msRedirectID := idgen.SourceID(srvRedirectID, "ms-2")
	for _, src := range []db.ItemSource{
		{SourceID: msProxyID, VirtualItemID: itemID, ServerID: srvProxyID, RemoteItemID: "rem-movie-1", RemoteMediaSourceID: "ms-1", Label: "[ProxyUp] 1080p"},
		{SourceID: msRedirectID, VirtualItemID: itemID, ServerID: srvRedirectID, RemoteItemID: "rem-movie-1", RemoteMediaSourceID: "ms-2", Label: "[RedirectUp] 1080p"},
	} {
		if err := env.database.UpsertSource(ctx, src); err != nil {
			t.Fatalf("UpsertSource: %v", err)
		}
	}

	// 1. Proxy mode with Range: bytes=0-1023 -> 206 Partial Content with 1024 bytes
	req := httptest.NewRequest(http.MethodGet, "/Videos/"+itemID+"/stream.mp4?Static=true&MediaSourceId="+msProxyID+"&api_key="+env.token, nil)
	req.Header.Set("Range", "bytes=0-1023")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 1024 || !bytes.Equal(rec.Body.Bytes(), payload[:1024]) {
		t.Fatalf("unexpected 206 payload len=%d", rec.Body.Len())
	}
	if rec.Header().Get("Content-Range") != "bytes 0-1023/4096" || rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("unexpected headers: Content-Range=%q Accept-Ranges=%q", rec.Header().Get("Content-Range"), rec.Header().Get("Accept-Ranges"))
	}

	// 2. HEAD request (defaulting to highest-priority source when MediaSourceId omitted)
	headReq := httptest.NewRequest(http.MethodHead, "/Videos/"+itemID+"/stream?api_key="+env.token, nil)
	headRec := httptest.NewRecorder()
	env.router.ServeHTTP(headRec, headReq)
	if headRec.Code != http.StatusOK || headRec.Body.Len() != 0 || headRec.Header().Get("Content-Length") != "4096" {
		t.Fatalf("HEAD failed: code=%d bodyLen=%d cl=%q", headRec.Code, headRec.Body.Len(), headRec.Header().Get("Content-Length"))
	}

	// 3. Redirect mode -> 302 Found
	redirReq := httptest.NewRequest(http.MethodGet, "/Videos/"+itemID+"/stream.mp4?MediaSourceId="+msRedirectID+"&api_key="+env.token, nil)
	redirRec := httptest.NewRecorder()
	env.router.ServeHTTP(redirRec, redirReq)
	if redirRec.Code != http.StatusFound {
		t.Fatalf("expected 302 Found in redirect mode, got %d", redirRec.Code)
	}
	loc := redirRec.Header().Get("Location")
	if !strings.HasPrefix(loc, upSrv.URL+"/Videos/rem-movie-1/stream") || !strings.Contains(loc, "ApiKey="+upToken) || !strings.Contains(loc, "mediaSourceId=ms-2") {
		t.Fatalf("unexpected redirect Location: %q", loc)
	}
}

func TestRewriteM3U8PreservesParamsAndStripsUpstreamToken(t *testing.T) {
	upURL, _ := url.Parse("http://upstream:8096/videos/remote-id-1/master.m3u8?MediaSourceId=ms1&PlaySessionId=remote-ps-99&api_key=upstream-secret-key")
	itemID := "0123456789abcdef0123456789abcdef"
	proxyPS := "fedcba9876543210fedcba9876543210"
	proxyTok := "proxy-user-token-abc"

	input := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:6",
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="English",URI="hls1/audio/main.m3u8?PlaySessionId=remote-ps-99&api_key=upstream-secret-key"`,
		"#EXT-X-STREAM-INF:BANDWIDTH=5000000",
		"main.m3u8?DeviceId=dev1&PlaySessionId=remote-ps-99&ApiKey=upstream-secret-key",
		`#EXT-X-MAP:URI="hls1/main/-1.mp4?runtimeTicks=0&actualSegmentLengthTicks=0&PlaySessionId=remote-ps-99&api_key=upstream-secret-key"`,
		"#EXTINF:6.0,",
		"http://upstream:8096/videos/remote-id-1/hls1/main/0.ts?PlaySessionId=remote-ps-99&api_key=upstream-secret-key",
		"/videos/remote-id-1/live.m3u8?PlaySessionId=remote-ps-99",
	}, "\n")

	outBytes, err := RewriteM3U8([]byte(input), upURL, itemID, proxyPS, proxyTok)
	if err != nil {
		t.Fatalf("RewriteM3U8: %v", err)
	}
	out := string(outBytes)

	if strings.Contains(out, "upstream-secret-key") {
		t.Fatalf("rewritten playlist leaked upstream api_key:\n%s", out)
	}

	// Verify #EXT-X-MEDIA URI="..." rewritten
	if !strings.Contains(out, `URI="/Videos/`+itemID+`/hls-proxy/videos/remote-id-1/hls1/audio/main.m3u8?`) {
		t.Fatalf("expected #EXT-X-MEDIA URI rewritten, got:\n%s", out)
	}
	// Verify #EXT-X-MAP URI="..." rewritten (critical for fMP4 HLS)
	if !strings.Contains(out, `URI="/Videos/`+itemID+`/hls-proxy/videos/remote-id-1/hls1/main/-1.mp4?`) {
		t.Fatalf("expected #EXT-X-MAP URI rewritten, got:\n%s", out)
	}
	// Verify sub-playlist, absolute URI, and root-relative URI all rewritten with PlaySessionId, mf_ps, and proxy api_key
	for _, needle := range []string{
		"/Videos/" + itemID + "/hls-proxy/videos/remote-id-1/main.m3u8?",
		"/Videos/" + itemID + "/hls-proxy/videos/remote-id-1/hls1/main/0.ts?",
		"/Videos/" + itemID + "/hls-proxy/videos/remote-id-1/live.m3u8?",
		"PlaySessionId=remote-ps-99",
		"mf_ps=" + proxyPS,
		"api_key=" + proxyTok,
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("missing %q in rewritten playlist:\n%s", needle, out)
		}
	}
}

func TestHandleHLSEndToEndMasterMainAndSegments(t *testing.T) {
	env := setupStreamEnv(t)
	ctx := context.Background()

	const upToken = "upstream-hls-secret"
	const remotePS = "remote-ps-xyz"
	initBytes := []byte("fmp4-init-segment-bytes")
	seg0Bytes := []byte("ts-or-fmp4-segment-0-bytes")

	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ApiKey") != upToken || r.Header.Get("X-Emby-Token") != upToken {
			http.Error(w, "bad upstream ApiKey/X-Emby-Token", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("mf_ps") != "" {
			http.Error(w, "mf_ps should not be forwarded to upstream", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/videos/rem-1/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(
				"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4000000\nhls1/main.m3u8?PlaySessionId=" + remotePS + "&api_key=" + upToken + "\n",
			))
		case "/videos/rem-1/hls1/main.m3u8":
			if r.URL.Query().Get("PlaySessionId") != remotePS {
				http.Error(w, "missing remote PlaySessionId", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(
				"#EXTM3U\n#EXT-X-MAP:URI=\"main/-1.mp4?PlaySessionId=" + remotePS + "&api_key=" + upToken + "\"\n#EXTINF:6.0,\nmain/0.ts?PlaySessionId=" + remotePS + "&api_key=" + upToken + "\n#EXT-X-ENDLIST\n",
			))
		case "/videos/rem-1/hls1/main/-1.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(initBytes)
		case "/videos/rem-1/hls1/main/0.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(seg0Bytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upSrv.Close()

	srvID := idgen.NewRandomHex32()
	encTok, _ := env.pool.EncryptToken(upToken)
	if err := env.database.UpsertServer(ctx, db.UpstreamServer{
		ID: srvID, Name: "HLSUp", BaseURL: upSrv.URL, Username: "u", RemoteUserID: "ru",
		AccessToken: encTok, Priority: 10, StreamMode: "proxy", Enabled: true, IsOnline: true,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}

	itemID := idgen.VirtualItemID("movie:tmdb:200")
	proxyPS := idgen.NewRandomHex32()
	if err := env.database.CreatePlaySession(ctx, db.ActivePlaySession{
		PlaySessionID:          proxyPS,
		UserID:                 env.userID,
		DeviceID:               "dev-stream-1",
		VirtualItemID:          itemID,
		SourceID:               idgen.SourceID(srvID, "ms-hls"),
		ServerID:               srvID,
		RemoteItemID:           "rem-1",
		RemotePlaySessionID:    remotePS,
		UpstreamTranscodingURL: upSrv.URL + "/videos/rem-1/master.m3u8?PlaySessionId=" + remotePS,
	}); err != nil {
		t.Fatalf("CreatePlaySession: %v", err)
	}

	// 1. Fetch master.m3u8
	mRec := httptest.NewRecorder()
	env.router.ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/Videos/"+itemID+"/master.m3u8?PlaySessionId="+proxyPS+"&api_key="+env.token, nil))
	if mRec.Code != http.StatusOK {
		t.Fatalf("master.m3u8 failed: %d %s", mRec.Code, mRec.Body.String())
	}
	masterBody := mRec.Body.String()
	if strings.Contains(masterBody, upToken) {
		t.Fatalf("master.m3u8 leaked upstream token:\n%s", masterBody)
	}

	var mainURL string
	for _, line := range strings.Split(masterBody, "\n") {
		if strings.HasPrefix(line, "/Videos/") {
			mainURL = line
			break
		}
	}
	if mainURL == "" {
		t.Fatalf("no rewritten sub-playlist URL in master.m3u8:\n%s", masterBody)
	}

	// 2. Follow rewritten main.m3u8 via HandleHLSProxy
	subRec := httptest.NewRecorder()
	env.router.ServeHTTP(subRec, httptest.NewRequest(http.MethodGet, mainURL, nil))
	if subRec.Code != http.StatusOK {
		t.Fatalf("hls-proxy main.m3u8 failed: %d %s", subRec.Code, subRec.Body.String())
	}
	subBody := subRec.Body.String()
	if strings.Contains(subBody, upToken) {
		t.Fatalf("main.m3u8 leaked upstream token:\n%s", subBody)
	}

	// Extract init segment URL from #EXT-X-MAP:URI="..." and media segment URL
	match := uriAttrRegex.FindStringSubmatch(subBody)
	if len(match) != 2 {
		t.Fatalf("missing rewritten #EXT-X-MAP URI in main.m3u8:\n%s", subBody)
	}
	initURL := match[1]

	var segURL string
	for _, line := range strings.Split(subBody, "\n") {
		if strings.HasPrefix(line, "/Videos/") {
			segURL = line
			break
		}
	}
	if segURL == "" {
		t.Fatalf("missing rewritten segment URL in main.m3u8:\n%s", subBody)
	}

	// 3. Fetch fMP4 init segment (-1.mp4) and media segment (0.ts)
	initRec := httptest.NewRecorder()
	env.router.ServeHTTP(initRec, httptest.NewRequest(http.MethodGet, initURL, nil))
	if initRec.Code != http.StatusOK || !bytes.Equal(initRec.Body.Bytes(), initBytes) {
		t.Fatalf("init segment failed: code=%d body=%q", initRec.Code, initRec.Body.String())
	}

	segRec := httptest.NewRecorder()
	env.router.ServeHTTP(segRec, httptest.NewRequest(http.MethodGet, segURL, nil))
	if segRec.Code != http.StatusOK || !bytes.Equal(segRec.Body.Bytes(), seg0Bytes) {
		t.Fatalf("media segment failed: code=%d body=%q", segRec.Code, segRec.Body.String())
	}
}
