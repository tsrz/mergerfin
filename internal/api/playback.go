package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"golang.org/x/sync/errgroup"
)

const (
	maxPlaybackInfoBodyBytes = 2 << 20
	playbackInfoTimeout      = 4 * time.Second
)

type playbackCandidate struct {
	src                 db.ItemSource
	srv                 db.UpstreamServer
	ms                  map[string]any
	remotePlaySessionID string
}

// handlePlaybackInfo negotiates playable media sources across active upstreams in parallel,
// scores/sorts candidates, persists active_play_sessions, and rewrites stream/HLS URLs.
func (h *apiHandler) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	ac, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	itemID, ok := normalizeRouteID(chi.URLParam(r, "itemId"))
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid item ID")
		return
	}

	ctx := r.Context()
	if _, err := h.db.GetItemByID(ctx, itemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Item not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to load item")
		return
	}

	sources, err := h.db.ListSourcesByVirtualItemID(ctx, itemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load item sources")
		return
	}

	servers, err := h.pool.ListEnabledDecryptedServers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load upstream servers")
		return
	}

	serverByID := selectActiveServers(servers)
	var activeSources []db.ItemSource
	for _, src := range sources {
		if _, ok := serverByID[src.ServerID]; ok {
			activeSources = append(activeSources, src)
		}
	}

	if len(activeSources) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"MediaSources":  []any{},
			"PlaySessionId": idgen.NewRandomHex32(),
		})
		return
	}

	bodyMap, err := readPlaybackBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}

	q := r.URL.Query()
	requestedMSID := firstNonEmpty(
		queryParamCI(q, "MediaSourceId"),
		mapStringCI(bodyMap, "MediaSourceId"),
	)
	if requestedMSID != "" {
		if norm, err := idgen.NormalizeID(requestedMSID); err == nil {
			requestedMSID = norm
			var filtered []db.ItemSource
			for _, src := range activeSources {
				if src.SourceID == requestedMSID {
					filtered = append(filtered, src)
				}
			}
			if len(filtered) > 0 {
				activeSources = filtered
			}
		}
	}

	deviceID := firstNonEmpty(
		queryParamCI(q, "DeviceId"),
		mapStringCI(bodyMap, "DeviceId"),
		ac.Client.DeviceID,
		ac.Session.DeviceID,
		"mergerfin-client",
	)

	callCtx, cancel := context.WithTimeout(ctx, playbackInfoTimeout)
	defer cancel()

	var (
		mu         sync.Mutex
		candidates []playbackCandidate
		g          errgroup.Group
	)

	for _, src := range activeSources {
		src := src
		srv := serverByID[src.ServerID]
		g.Go(func() error {
			fwdQuery := cloneQueryWithoutCI(q, "UserId", "MediaSourceId")
			if srv.RemoteUserID != "" {
				fwdQuery.Set("UserId", srv.RemoteUserID)
			}
			fwdQuery.Set("MediaSourceId", src.RemoteMediaSourceID)

			fwdBody, err := buildUpstreamPlaybackBody(bodyMap, srv.RemoteUserID, src.RemoteMediaSourceID)
			if err != nil {
				h.logger.Warn("marshal upstream playback body failed", "server_id", srv.ID, "error", err)
				return nil
			}

			respBytes, status, err := h.client.PostPlaybackInfo(callCtx, srv, src.RemoteItemID, fwdQuery, fwdBody)
			if err != nil {
				h.logger.Warn("upstream PlaybackInfo request failed", "server_id", srv.ID, "server_name", srv.Name, "error", err)
				return nil
			}
			if status < 200 || status >= 300 {
				h.logger.Warn("upstream PlaybackInfo non-2xx", "server_id", srv.ID, "status", status)
				return nil
			}

			var parsed struct {
				MediaSources  []map[string]any `json:"MediaSources"`
				PlaySessionID string           `json:"PlaySessionId"`
			}
			if err := json.Unmarshal(respBytes, &parsed); err != nil {
				h.logger.Warn("decode upstream PlaybackInfo failed", "server_id", srv.ID, "error", err)
				return nil
			}

			matched := selectUpstreamMediaSources(parsed.MediaSources, src.RemoteMediaSourceID)
			mu.Lock()
			for _, ms := range matched {
				candidates = append(candidates, playbackCandidate{
					src:                 src,
					srv:                 srv,
					ms:                  ms,
					remotePlaySessionID: parsed.PlaySessionID,
				})
			}
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	if len(candidates) == 0 {
		writeError(w, http.StatusBadGateway, "All upstream playback negotiations failed")
		return
	}

	sortPlaybackCandidates(candidates, requestedMSID)

	userID := ac.User.ID
	proxyToken := ac.Session.Token

	rewritten := make([]map[string]any, 0, len(candidates))
	var topPlaySessionID string

	for _, cand := range candidates {
		proxyPlaySessionID := idgen.NewRandomHex32()
		if topPlaySessionID == "" {
			topPlaySessionID = proxyPlaySessionID
		}

		rawTranscodingURL := mapString(cand.ms, "TranscodingUrl")
		upstreamTranscodingURL := resolveUpstreamURL(cand.srv.BaseURL, rawTranscodingURL)
		remotePS := extractPlaySessionIDFromURL(upstreamTranscodingURL)
		if remotePS == "" {
			remotePS = cand.remotePlaySessionID
		}

		if err := h.db.CreatePlaySession(ctx, db.ActivePlaySession{
			PlaySessionID:          proxyPlaySessionID,
			UserID:                 userID,
			DeviceID:               deviceID,
			VirtualItemID:          itemID,
			SourceID:               cand.src.SourceID,
			ServerID:               cand.srv.ID,
			RemoteItemID:           cand.src.RemoteItemID,
			RemotePlaySessionID:    remotePS,
			UpstreamTranscodingURL: upstreamTranscodingURL,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to persist playback session")
			return
		}

		container := firstNonEmpty(mapString(cand.ms, "Container"), cand.src.Container)
		ext := cleanContainerExt(container, mapString(cand.ms, "Path"))

		cand.ms["Id"] = cand.src.SourceID
		cand.ms["Name"] = cand.src.Label
		cand.ms["DirectStreamUrl"] = fmt.Sprintf(
			"/Videos/%s/stream.%s?Static=true&MediaSourceId=%s&PlaySessionId=%s&DeviceId=%s&api_key=%s",
			itemID, ext, cand.src.SourceID, proxyPlaySessionID, url.QueryEscape(deviceID), url.QueryEscape(proxyToken),
		)

		if mapBool(cand.ms, "SupportsTranscoding") && upstreamTranscodingURL != "" {
			cand.ms["TranscodingUrl"] = fmt.Sprintf(
				"/Videos/%s/master.m3u8?MediaSourceId=%s&PlaySessionId=%s&DeviceId=%s&api_key=%s",
				itemID, cand.src.SourceID, proxyPlaySessionID, url.QueryEscape(deviceID), url.QueryEscape(proxyToken),
			)
			if mapString(cand.ms, "TranscodingSubProtocol") == "" {
				cand.ms["TranscodingSubProtocol"] = "hls"
			}
		} else {
			delete(cand.ms, "TranscodingUrl")
		}

		rewritten = append(rewritten, cand.ms)
	}

	if topPlaySessionID == "" {
		topPlaySessionID = idgen.NewRandomHex32()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"MediaSources":  rewritten,
		"PlaySessionId": topPlaySessionID,
	})
}

// handleStopActiveEncodings forwards transcoding cancellation to the responsible upstream
// server(s) and cleans up matching active_play_sessions rows.
func (h *apiHandler) handleStopActiveEncodings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	playSessionID := queryParamCI(q, "PlaySessionId")
	deviceID := queryParamCI(q, "DeviceId")

	if playSessionID != "" {
		ps, err := h.db.GetPlaySession(ctx, playSessionID)
		if err == nil && ps != nil {
			h.stopAndDeleteSession(ctx, *ps)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if deviceID != "" {
		rows, err := h.db.SqlDB().QueryContext(ctx, `
			SELECT play_session_id, user_id, device_id, virtual_item_id, source_id,
			       server_id, remote_item_id, remote_play_session_id, upstream_transcoding_url, started_at
			FROM active_play_sessions
			WHERE device_id = ?
		`, deviceID)
		if err == nil {
			var sessions []db.ActivePlaySession
			for rows.Next() {
				var ps db.ActivePlaySession
				if scanErr := rows.Scan(
					&ps.PlaySessionID, &ps.UserID, &ps.DeviceID, &ps.VirtualItemID, &ps.SourceID,
					&ps.ServerID, &ps.RemoteItemID, &ps.RemotePlaySessionID, &ps.UpstreamTranscodingURL, &ps.StartedAt,
				); scanErr == nil {
					sessions = append(sessions, ps)
				}
			}
			_ = rows.Close()

			for _, ps := range sessions {
				h.stopAndDeleteSession(ctx, ps)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandler) stopAndDeleteSession(ctx context.Context, ps db.ActivePlaySession) {
	if ps.ServerID != "" {
		if srv, err := h.pool.GetDecryptedServer(ctx, ps.ServerID); err == nil && srv != nil {
			if err := h.client.StopActiveEncodings(ctx, *srv, ps.DeviceID, ps.RemotePlaySessionID); err != nil {
				h.logger.Debug("upstream StopActiveEncodings failed", "server_id", srv.ID, "play_session_id", ps.PlaySessionID, "error", err)
			}
		}
	}
	_ = h.db.DeletePlaySession(ctx, ps.PlaySessionID)
}

func selectActiveServers(servers []db.UpstreamServer) map[string]db.UpstreamServer {
	online := make(map[string]db.UpstreamServer)
	for _, srv := range servers {
		if srv.IsOnline {
			online[srv.ID] = srv
		}
	}
	if len(online) > 0 {
		return online
	}
	all := make(map[string]db.UpstreamServer, len(servers))
	for _, srv := range servers {
		all[srv.ID] = srv
	}
	return all
}

func readPlaybackBody(r *http.Request) (map[string]any, error) {
	if r.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPlaybackInfoBodyBytes))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func buildUpstreamPlaybackBody(bodyMap map[string]any, remoteUserID, remoteMediaSourceID string) ([]byte, error) {
	cloned := make(map[string]any, len(bodyMap)+2)
	for k, v := range bodyMap {
		if strings.EqualFold(k, "MediaSourceId") || strings.EqualFold(k, "UserId") {
			continue
		}
		cloned[k] = v
	}
	cloned["MediaSourceId"] = remoteMediaSourceID
	if remoteUserID != "" {
		cloned["UserId"] = remoteUserID
	}
	return json.Marshal(cloned)
}

func selectUpstreamMediaSources(sources []map[string]any, remoteMediaSourceID string) []map[string]any {
	if len(sources) <= 1 {
		return sources
	}
	for _, ms := range sources {
		if mapString(ms, "Id") == remoteMediaSourceID {
			return []map[string]any{ms}
		}
	}
	return sources
}

func sortPlaybackCandidates(candidates []playbackCandidate, requestedMSID string) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]

		aReq := requestedMSID != "" && a.src.SourceID == requestedMSID
		bReq := requestedMSID != "" && b.src.SourceID == requestedMSID
		if aReq != bReq {
			return aReq
		}

		aDP := mapBool(a.ms, "SupportsDirectPlay")
		bDP := mapBool(b.ms, "SupportsDirectPlay")
		if aDP != bDP {
			return aDP
		}

		aDS := mapBool(a.ms, "SupportsDirectStream")
		bDS := mapBool(b.ms, "SupportsDirectStream")
		if aDS != bDS {
			return aDS
		}

		if a.srv.Priority != b.srv.Priority {
			return a.srv.Priority < b.srv.Priority
		}

		aBitrate := mapInt64(a.ms, "Bitrate")
		if aBitrate == 0 {
			aBitrate = a.src.Bitrate
		}
		bBitrate := mapInt64(b.ms, "Bitrate")
		if bBitrate == 0 {
			bBitrate = b.src.Bitrate
		}
		return aBitrate > bBitrate
	})
}

func cleanContainerExt(container, path string) string {
	lower := strings.ToLower(strings.TrimSpace(container))
	if lower != "" {
		tokens := strings.Split(lower, ",")
		tokenSet := make(map[string]struct{}, len(tokens))
		first := ""
		for _, raw := range tokens {
			tok := strings.TrimSpace(raw)
			if tok == "" {
				continue
			}
			if first == "" {
				first = tok
			}
			tokenSet[tok] = struct{}{}
		}
		has := func(k string) bool {
			_, ok := tokenSet[k]
			return ok
		}
		switch {
		case has("mp4"):
			return "mp4"
		case has("matroska") || has("mkv"):
			return "mkv"
		case has("webm"):
			return "webm"
		case has("mpegts") || has("ts"):
			return "ts"
		case has("avi"):
			return "avi"
		case has("mov"):
			return "mov"
		case isAlphaNum(first):
			return first
		}
	}

	if ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."); ext != "" && isAlphaNum(ext) {
		return ext
	}
	return "mp4"
}

func isAlphaNum(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func resolveUpstreamURL(baseURL, raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return trimmed
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasPrefix(trimmed, "/") {
		return base + trimmed
	}
	return base + "/" + trimmed
}

func extractPlaySessionIDFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return queryParamCI(u.Query(), "PlaySessionId")
}

func cloneQueryWithoutCI(q url.Values, omitKeys ...string) url.Values {
	out := make(url.Values, len(q))
	for k, vals := range q {
		skip := false
		for _, omit := range omitKeys {
			if strings.EqualFold(k, omit) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}

func queryParamCI(q url.Values, key string) string {
	if q == nil {
		return ""
	}
	if v := strings.TrimSpace(q.Get(key)); v != "" {
		return v
	}
	for k, vals := range q {
		if strings.EqualFold(k, key) && len(vals) > 0 {
			if v := strings.TrimSpace(vals[0]); v != "" {
				return v
			}
		}
	}
	return ""
}

func mapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func mapStringCI(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v := mapString(m, key); v != "" {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func mapBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

func mapInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		parsed, _ := n.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(n, 10, 64)
		return parsed
	default:
		return 0
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
