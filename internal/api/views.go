package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/auth"
)

func resolveRequestUserID(r *http.Request) (string, bool) {
	if raw := strings.TrimSpace(chi.URLParam(r, "userId")); raw != "" {
		if norm, ok := normalizeRouteID(raw); ok {
			return norm, true
		}
		return "", false
	}
	if qUser := queryFirst(r, "userId", "UserId"); qUser != "" {
		if norm, ok := normalizeRouteID(qUser); ok {
			return norm, true
		}
		return "", false
	}
	if ac, ok := auth.FromContext(r.Context()); ok {
		return ac.User.ID, true
	}
	return "", true
}

func (h *apiHandler) handleUserViews(w http.ResponseWriter, r *http.Request) {
	userID, valid := resolveRequestUserID(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	libs, err := h.db.ListLibraries(r.Context())
	if err != nil {
		h.logger.Error("failed to list virtual libraries", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list views")
		return
	}

	serverID, _ := h.serverIdentity(r.Context())
	items := make([]BaseItemDto, 0, len(libs))
	for _, lib := range libs {
		items = append(items, libraryToDTO(r.Context(), h.db, serverID, userID, lib))
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            items,
		TotalRecordCount: len(items),
		StartIndex:       0,
	})
}
