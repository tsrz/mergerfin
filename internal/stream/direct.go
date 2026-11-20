package stream

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

const streamCopyBufferBytes = 64 * 1024

// Streamer proxies or redirects Jellyfin direct video streams and dynamic HLS playlists/segments.
type Streamer struct {
	db         *db.DB
	pool       *upstream.Pool
	logger     *slog.Logger
	httpClient *http.Client
}

// NewStreamer creates a Streamer with a streaming-safe http.Client (no overall request timeout,
// bounded dial and response-header timeouts, and no automatic redirect following).
func NewStreamer(database *db.DB, pool *upstream.Pool, logger *slog.Logger) *Streamer {
	if logger == nil {
		logger = slog.Default()
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
	}

	return &Streamer{
		db:     database,
		pool:   pool,
		logger: logger,
		httpClient: &http.Client{
			Timeout:   0,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// HandleDirectStream serves GET|HEAD /Videos/{itemId}/stream and /Videos/{itemId}/stream.{ext}
// either by issuing a 302 redirect (StreamMode == "redirect") or zero-copy streaming with
// Range / 206 Partial Content support (StreamMode == "proxy").
func (s *Streamer) HandleDirectStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	itemID, err := idgen.NormalizeID(chi.URLParam(r, "itemId"))
	if err != nil {
		http.Error(w, "invalid itemId", http.StatusBadRequest)
		return
	}

	var src *db.ItemSource
	rawMSID := queryFirst(r, "MediaSourceId", "mediaSourceId")
	if rawMSID != "" {
		msID, err := idgen.NormalizeID(rawMSID)
		if err != nil {
			http.Error(w, "invalid MediaSourceId", http.StatusBadRequest)
			return
		}
		found, err := s.db.GetSourceByID(ctx, msID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "media source not found", http.StatusNotFound)
				return
			}
			s.logger.Error("lookup source failed", "source_id", msID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if found.VirtualItemID != itemID {
			http.Error(w, "media source does not belong to item", http.StatusNotFound)
			return
		}
		src = found
	} else {
		sources, err := s.db.ListSourcesByVirtualItemID(ctx, itemID)
		if err != nil {
			s.logger.Error("list sources failed", "item_id", itemID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if len(sources) == 0 {
			http.Error(w, "no media sources available", http.StatusNotFound)
			return
		}
		src = &sources[0]
	}

	srv, err := s.pool.GetDecryptedServer(ctx, src.ServerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "upstream server not found", http.StatusNotFound)
			return
		}
		s.logger.Error("decrypt upstream server failed", "server_id", src.ServerID, "error", err)
		http.Error(w, "upstream server unavailable", http.StatusBadGateway)
		return
	}

	targetURL, err := buildDirectUpstreamURL(srv, src, r.URL.Query())
	if err != nil {
		s.logger.Error("build upstream direct stream url failed", "error", err)
		http.Error(w, "invalid upstream URL", http.StatusBadGateway)
		return
	}

	if strings.EqualFold(srv.StreamMode, "redirect") {
		http.Redirect(w, r, targetURL, http.StatusFound)
		return
	}

	upReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, nil)
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
	if ifRng := r.Header.Get("If-Range"); ifRng != "" {
		upReq.Header.Set("If-Range", ifRng)
	}

	resp, err := s.httpClient.Do(upReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		s.logger.Warn("upstream direct stream request failed", "server_id", srv.ID, "error", err)
		http.Error(w, "upstream stream failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyStreamHeaders(w.Header(), resp.Header)
	if (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) && w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}

	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}

	streamZeroCopy(ctx, w, resp.Body, s.logger)
}

func buildDirectUpstreamURL(srv *db.UpstreamServer, src *db.ItemSource, incomingQuery url.Values) (string, error) {
	base := strings.TrimRight(srv.BaseURL, "/")
	u, err := url.Parse(base + "/Videos/" + url.PathEscape(src.RemoteItemID) + "/stream")
	if err != nil {
		return "", err
	}

	q := url.Values{}
	q.Set("static", "true")
	q.Set("mediaSourceId", src.RemoteMediaSourceID)
	q.Set("ApiKey", srv.AccessToken)
	if tag := queryVal(incomingQuery, "Tag", "tag"); tag != "" {
		q.Set("Tag", tag)
	}
	if ticks := queryVal(incomingQuery, "StartTimeTicks", "startTimeTicks"); ticks != "" {
		q.Set("StartTimeTicks", ticks)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func copyStreamHeaders(dst, src http.Header) {
	for _, h := range []string{
		"Content-Type",
		"Content-Length",
		"Content-Range",
		"Accept-Ranges",
		"ETag",
		"Last-Modified",
		"Cache-Control",
	} {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
}

func streamZeroCopy(ctx context.Context, w http.ResponseWriter, body io.Reader, logger *slog.Logger) {
	buf := make([]byte, streamCopyBufferBytes)
	rc := http.NewResponseController(w)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			_ = rc.Flush()
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && !errors.Is(ctx.Err(), context.Canceled) {
				logger.Debug("upstream stream ended with error", "error", readErr)
			}
			return
		}
	}
}

func queryFirst(r *http.Request, keys ...string) string {
	return queryVal(r.URL.Query(), keys...)
}

func queryVal(q url.Values, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			return v
		}
	}
	return ""
}
