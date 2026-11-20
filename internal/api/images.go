package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

func (h *apiHandler) imageCacheDir() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.ImageCacheDir()) != "" {
		return h.cfg.ImageCacheDir()
	}
	return filepath.Join(os.TempDir(), "mergerfin-image-cache")
}

func buildImageCacheKey(itemID, imageType, imageIndex, maxWidth, maxHeight, quality, format string) string {
	raw := strings.Join([]string{
		itemID,
		strings.ToLower(imageType),
		imageIndex,
		maxWidth,
		maxHeight,
		quality,
		strings.ToLower(format),
	}, "|")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func writeFileAtomic(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%s", dst, idgen.NewRandomHex32())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (h *apiHandler) handleGetItemImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	itemID, ok := normalizeRouteID(chi.URLParam(r, "itemId"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid itemId")
		return
	}

	imageType := strings.TrimSpace(chi.URLParam(r, "imageType"))
	if imageType == "" {
		imageType = "Primary"
	}
	imageIndex := strings.TrimSpace(chi.URLParam(r, "imageIndex"))
	if imageIndex == "" {
		imageIndex = "0"
	}

	item, err := h.db.GetItemByID(ctx, itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "item not found")
			return
		}
		h.logger.Error("lookup item for image failed", "item_id", itemID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to lookup item")
		return
	}

	serverID := item.ImgPrimaryServerID
	remoteID := item.ImgPrimaryRemoteID
	upstreamType := imageType

	if strings.EqualFold(imageType, "Backdrop") {
		if item.ImgBackdropServerID != "" && item.ImgBackdropRemoteID != "" {
			serverID = item.ImgBackdropServerID
			remoteID = item.ImgBackdropRemoteID
		} else if item.ImgPrimaryServerID != "" && item.ImgPrimaryRemoteID != "" {
			serverID = item.ImgPrimaryServerID
			remoteID = item.ImgPrimaryRemoteID
			upstreamType = "Primary"
		}
	}

	if serverID == "" || remoteID == "" {
		writeError(w, http.StatusNotFound, "image not available")
		return
	}

	q := r.URL.Query()
	cacheKey := buildImageCacheKey(
		itemID,
		imageType,
		imageIndex,
		queryVal(q, "maxWidth", "MaxWidth"),
		queryVal(q, "maxHeight", "MaxHeight"),
		queryVal(q, "quality", "Quality"),
		queryVal(q, "format", "Format"),
	)

	cacheDir := h.imageCacheDir()
	binPath := filepath.Join(cacheDir, cacheKey+".bin")
	ctPath := filepath.Join(cacheDir, cacheKey+".ct")

	etag := `"` + cacheKey + `"`
	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" && inm == etag {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Cache hit
	if data, err := os.ReadFile(binPath); err == nil && len(data) > 0 {
		ct := ""
		if ctBytes, ctErr := os.ReadFile(ctPath); ctErr == nil {
			ct = strings.TrimSpace(string(ctBytes))
		}
		if ct == "" {
			ct = http.DetectContentType(data)
		}
		serveCachedImage(w, r, ct, etag, data)
		return
	}

	// Cache miss: fetch from upstream
	if h.pool == nil {
		writeError(w, http.StatusServiceUnavailable, "upstream pool unavailable")
		return
	}
	srv, err := h.pool.GetDecryptedServer(ctx, serverID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "upstream server not found")
			return
		}
		h.logger.Error("decrypt upstream server for image failed", "server_id", serverID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream server unavailable")
		return
	}

	client := h.client
	if client == nil {
		client = upstream.NewClient()
	}

	data, contentType, status, err := client.FetchImage(ctx, *srv, remoteID, upstreamType, q)
	if err != nil {
		h.logger.Warn("fetch upstream image failed", "item_id", itemID, "server_id", serverID, "status", status, "error", err)
		if status == http.StatusNotFound {
			writeError(w, http.StatusNotFound, "upstream image not found")
			return
		}
		writeError(w, http.StatusBadGateway, "failed to fetch upstream image")
		return
	}
	if status != http.StatusOK || len(data) == 0 {
		if status == http.StatusNotFound {
			writeError(w, http.StatusNotFound, "upstream image not found")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream image request failed")
		return
	}

	if strings.TrimSpace(contentType) == "" {
		contentType = http.DetectContentType(data)
	}

	if err := writeFileAtomic(ctPath, []byte(contentType)); err != nil {
		h.logger.Warn("cache image content-type write failed", "error", err)
	}
	if err := writeFileAtomic(binPath, data); err != nil {
		h.logger.Warn("cache image payload write failed", "error", err)
	}

	serveCachedImage(w, r, contentType, etag, data)
}

func serveCachedImage(w http.ResponseWriter, r *http.Request, contentType, etag string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
