package stream

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
)

const maxPlaylistBytes = 16 << 20

var uriAttrRegex = regexp.MustCompile(`URI="([^"]+)"`)

// HandleHLSPlaylist serves GET /Videos/{itemId}/master.m3u8, main.m3u8, and live.m3u8
// by fetching the upstream playlist for the active play session and rewriting all URIs
// to route through /Videos/{itemId}/hls-proxy/*.
func (s *Streamer) HandleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	itemID, err := idgen.NormalizeID(chi.URLParam(r, "itemId"))
	if err != nil {
		http.Error(w, "invalid itemId", http.StatusBadRequest)
		return
	}

	ps, err := s.resolvePlaySession(ctx, r, itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "play session not found", http.StatusNotFound)
			return
		}
		s.logger.Error("resolve play session failed", "item_id", itemID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if strings.TrimSpace(ps.UpstreamTranscodingURL) == "" {
		http.Error(w, "upstream transcoding URL not available", http.StatusNotFound)
		return
	}

	srv, err := s.pool.GetDecryptedServer(ctx, ps.ServerID)
	if err != nil {
		s.logger.Error("decrypt upstream server failed", "server_id", ps.ServerID, "error", err)
		http.Error(w, "upstream server unavailable", http.StatusBadGateway)
		return
	}

	upURL, err := url.Parse(ps.UpstreamTranscodingURL)
	if err != nil {
		http.Error(w, "invalid upstream transcoding URL", http.StatusBadGateway)
		return
	}

	reqBase := path.Base(r.URL.Path)
	if (reqBase == "main.m3u8" || reqBase == "live.m3u8") && strings.HasSuffix(upURL.Path, "/master.m3u8") {
		upURL.Path = strings.TrimSuffix(upURL.Path, "master.m3u8") + reqBase
	}

	upQuery := upURL.Query()
	if srv.AccessToken != "" {
		upQuery.Del("api_key")
		if strings.TrimSpace(upQuery.Get("ApiKey")) == "" {
			upQuery.Set("ApiKey", srv.AccessToken)
		}
		upURL.RawQuery = upQuery.Encode()
	}

	proxyToken := extractProxyToken(r)
	s.fetchAndServePlaylist(ctx, w, upURL, srv.AccessToken, itemID, ps.PlaySessionID, proxyToken)
}

// HandleHLSProxy serves GET /Videos/{itemId}/hls-proxy/* by proxying sub-playlists
// (rewritten via RewriteM3U8) and media segments (zero-copy) from the upstream server.
func (s *Streamer) HandleHLSProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	itemID, err := idgen.NormalizeID(chi.URLParam(r, "itemId"))
	if err != nil {
		http.Error(w, "invalid itemId", http.StatusBadRequest)
		return
	}

	upstreamRelPath := strings.TrimLeft(chi.URLParam(r, "*"), "/")
	if upstreamRelPath == "" || strings.Contains(upstreamRelPath, "..") {
		http.Error(w, "invalid hls-proxy path", http.StatusBadRequest)
		return
	}

	ps, err := s.resolvePlaySession(ctx, r, itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "play session not found", http.StatusNotFound)
			return
		}
		s.logger.Error("resolve play session for hls-proxy failed", "item_id", itemID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	srv, err := s.pool.GetDecryptedServer(ctx, ps.ServerID)
	if err != nil {
		s.logger.Error("decrypt upstream server failed", "server_id", ps.ServerID, "error", err)
		http.Error(w, "upstream server unavailable", http.StatusBadGateway)
		return
	}

	upURL, err := buildHLSProxyUpstreamURL(srv, upstreamRelPath, r.URL.Query())
	if err != nil {
		http.Error(w, "invalid upstream URL", http.StatusBadGateway)
		return
	}

	upReq, err := http.NewRequestWithContext(ctx, r.Method, upURL.String(), nil)
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusBadGateway)
		return
	}
	if srv.AccessToken != "" {
		upReq.Header.Set("X-Emby-Token", srv.AccessToken)
	}
	if rng := r.Header.Get("Range"); rng != "" {
		upReq.Header.Set("Range", rng)
	}

	resp, err := s.httpClient.Do(upReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		s.logger.Warn("upstream hls-proxy request failed", "url", upURL.Redacted(), "error", err)
		http.Error(w, "upstream HLS request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	isPlaylist := strings.Contains(ct, "mpegurl") || strings.HasSuffix(strings.ToLower(upstreamRelPath), ".m3u8")

	if isPlaylist {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			http.Error(w, "upstream playlist error", resp.StatusCode)
			return
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaylistBytes))
		if err != nil {
			http.Error(w, "failed to read upstream playlist", http.StatusBadGateway)
			return
		}
		rewritten, err := RewriteM3U8(body, upURL, itemID, ps.PlaySessionID, extractProxyToken(r))
		if err != nil {
			http.Error(w, "failed to rewrite playlist", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(rewritten)
		}
		return
	}

	copyStreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	streamZeroCopy(ctx, w, resp.Body, s.logger)
}

func (s *Streamer) fetchAndServePlaylist(ctx context.Context, w http.ResponseWriter, upURL *url.URL, upstreamToken, itemID, proxyPlaySessionID, proxyToken string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upURL.String(), nil)
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusBadGateway)
		return
	}
	if upstreamToken != "" {
		req.Header.Set("X-Emby-Token", upstreamToken)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		s.logger.Warn("fetch upstream HLS playlist failed", "error", err)
		http.Error(w, "upstream HLS playlist failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "upstream playlist error", resp.StatusCode)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaylistBytes))
	if err != nil {
		http.Error(w, "failed to read upstream playlist", http.StatusBadGateway)
		return
	}

	rewritten, err := RewriteM3U8(body, upURL, itemID, proxyPlaySessionID, proxyToken)
	if err != nil {
		http.Error(w, "failed to rewrite playlist", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
}

func (s *Streamer) resolvePlaySession(ctx context.Context, r *http.Request, itemID string) (*db.ActivePlaySession, error) {
	if psID := queryFirst(r, "mf_ps", "PlaySessionId", "playSessionId"); psID != "" {
		if ps, err := s.db.GetPlaySession(ctx, psID); err == nil {
			return ps, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	userID := ""
	deviceID := queryFirst(r, "DeviceId", "deviceId")
	if ac, ok := auth.FromContext(ctx); ok {
		userID = ac.User.ID
		if deviceID == "" {
			deviceID = ac.Client.DeviceID
		}
		if deviceID == "" {
			deviceID = ac.Session.DeviceID
		}
	}

	if userID != "" && deviceID != "" {
		return s.db.GetLatestPlaySessionByDevice(ctx, userID, deviceID, itemID)
	}
	return nil, sql.ErrNoRows
}

func extractProxyToken(r *http.Request) string {
	if ac, ok := auth.FromContext(r.Context()); ok && ac.Session.Token != "" {
		return ac.Session.Token
	}
	return auth.ParseRequest(r).Token
}

func buildHLSProxyUpstreamURL(srv *db.UpstreamServer, upstreamRelPath string, incomingQuery url.Values) (*url.URL, error) {
	base := strings.TrimRight(srv.BaseURL, "/")
	rel := strings.TrimLeft(upstreamRelPath, "/")

	if baseParsed, err := url.Parse(base); err == nil {
		basePath := strings.Trim(baseParsed.Path, "/")
		if basePath != "" && strings.HasPrefix(rel, basePath+"/") {
			baseParsed.Path = "/" + rel
			baseParsed.RawQuery = ""
			base = baseParsed.String()
		} else {
			base = base + "/" + rel
		}
	} else {
		base = base + "/" + rel
	}

	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	for k, vals := range incomingQuery {
		if strings.EqualFold(k, "mf_ps") || strings.EqualFold(k, "api_key") || strings.EqualFold(k, "ApiKey") {
			continue
		}
		for _, v := range vals {
			q.Add(k, v)
		}
	}
	if srv.AccessToken != "" {
		q.Set("ApiKey", srv.AccessToken)
	}
	u.RawQuery = q.Encode()
	return u, nil
}

// RewriteM3U8 rewrites all segment, sub-playlist, and #EXT-X-* URI="..." references inside an
// M3U8 playlist so they route through /Videos/{virtualItemID}/hls-proxy/* without leaking
// upstream API keys, while preserving upstream query parameters (such as upstream PlaySessionId)
// alongside mf_ps and the local proxy api_key.
func RewriteM3U8(playlist []byte, upstreamPlaylistURL *url.URL, virtualItemID, proxyPlaySessionID, proxyToken string) ([]byte, error) {
	rewriteURI := func(rawURI string) string {
		trimmed := strings.TrimSpace(rawURI)
		if trimmed == "" || upstreamPlaylistURL == nil {
			return rawURI
		}

		ref, err := url.Parse(trimmed)
		if err != nil {
			return rawURI
		}
		resolved := upstreamPlaylistURL.ResolveReference(ref)

		q := url.Values{}
		for k, vals := range resolved.Query() {
			if strings.EqualFold(k, "api_key") || strings.EqualFold(k, "ApiKey") || strings.EqualFold(k, "mf_ps") {
				continue
			}
			for _, v := range vals {
				q.Add(k, v)
			}
		}
		if proxyPlaySessionID != "" {
			q.Set("mf_ps", proxyPlaySessionID)
		}
		if proxyToken != "" {
			q.Set("api_key", proxyToken)
		}

		relPath := strings.TrimLeft(resolved.Path, "/")
		out := "/Videos/" + virtualItemID + "/hls-proxy/" + relPath
		if encoded := q.Encode(); encoded != "" {
			out += "?" + encoded
		}
		return out
	}

	lines := strings.Split(string(playlist), "\n")
	var out bytes.Buffer
	out.Grow(len(playlist) + 512)

	for i, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			// Preserve blank lines.
		case strings.HasPrefix(trimmed, "#"):
			if strings.Contains(line, `URI="`) {
				line = uriAttrRegex.ReplaceAllStringFunc(line, func(match string) string {
					sub := uriAttrRegex.FindStringSubmatch(match)
					if len(sub) != 2 {
						return match
					}
					return `URI="` + rewriteURI(sub[1]) + `"`
				})
			}
			out.WriteString(line)
		default:
			out.WriteString(rewriteURI(trimmed))
		}

		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}

	return out.Bytes(), nil
}
