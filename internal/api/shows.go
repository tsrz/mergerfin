package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/db"
)

func (h *apiHandler) handleGetSeasons(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, valid := resolveRequestUserID(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	seriesID, ok := normalizeRouteID(chi.URLParam(r, "seriesId"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid seriesId")
		return
	}

	seasons, err := h.db.GetSeasons(ctx, seriesID)
	if err != nil {
		h.logger.Error("get seasons failed", "series_id", seriesID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query seasons")
		return
	}

	serverID, _ := h.serverIdentity(ctx)
	dtos, err := buildItemDTOs(ctx, h.db, serverID, userID, seasons, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to format seasons")
		return
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            dtos,
		TotalRecordCount: len(dtos),
		StartIndex:       0,
	})
}

func (h *apiHandler) handleGetEpisodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, valid := resolveRequestUserID(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	seriesID, ok := normalizeRouteID(chi.URLParam(r, "seriesId"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid seriesId")
		return
	}

	q := r.URL.Query()
	var seasonID string
	if rawSeasonID := queryVal(q, "SeasonId", "seasonId"); rawSeasonID != "" {
		norm, ok := normalizeRouteID(rawSeasonID)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid SeasonId")
			return
		}
		seasonID = norm
	}

	episodes, err := h.db.GetEpisodes(ctx, seriesID, seasonID)
	if err != nil {
		h.logger.Error("get episodes failed", "series_id", seriesID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query episodes")
		return
	}

	if rawSeasonNum := queryVal(q, "Season", "season"); rawSeasonNum != "" && seasonID == "" {
		if seasonNum, err := strconv.Atoi(rawSeasonNum); err == nil {
			filtered := make([]db.VirtualItem, 0, len(episodes))
			for _, ep := range episodes {
				if ep.ParentIndexNumber == seasonNum {
					filtered = append(filtered, ep)
				}
			}
			episodes = filtered
		}
	}

	total := len(episodes)
	startIndex := max(0, parseOptionalInt(queryVal(q, "StartIndex", "startIndex"), 0))
	limit := max(0, parseOptionalInt(queryVal(q, "Limit", "limit"), 0))

	if startIndex < len(episodes) {
		episodes = episodes[startIndex:]
		if limit > 0 && len(episodes) > limit {
			episodes = episodes[:limit]
		}
	} else {
		episodes = nil
	}

	serverID, _ := h.serverIdentity(ctx)
	dtos, err := buildItemDTOs(ctx, h.db, serverID, userID, episodes, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to format episodes")
		return
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            dtos,
		TotalRecordCount: total,
		StartIndex:       startIndex,
	})
}

func (h *apiHandler) handleGetNextUp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, valid := resolveRequestUserID(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	q := r.URL.Query()
	var seriesID string
	if rawSeries := queryVal(q, "SeriesId", "seriesId"); rawSeries != "" {
		norm, ok := normalizeRouteID(rawSeries)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid SeriesId")
			return
		}
		seriesID = norm
	}

	startIndex := max(0, parseOptionalInt(queryVal(q, "StartIndex", "startIndex"), 0))
	limit := parseOptionalInt(queryVal(q, "Limit", "limit"), 24)
	if limit <= 0 {
		limit = 24
	}

	items, err := h.db.GetNextUpEpisodes(ctx, userID, seriesID, limit+startIndex)
	if err != nil {
		h.logger.Error("get next up failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query next up")
		return
	}

	total := len(items)
	if startIndex < len(items) {
		items = items[startIndex:]
		if len(items) > limit {
			items = items[:limit]
		}
	} else {
		items = nil
	}

	serverID, _ := h.serverIdentity(ctx)
	dtos, err := buildItemDTOs(ctx, h.db, serverID, userID, items, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to format next up")
		return
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            dtos,
		TotalRecordCount: total,
		StartIndex:       startIndex,
	})
}
