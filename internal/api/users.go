package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
)

const (
	defaultAuthProviderID          = "Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider"
	defaultPasswordResetProviderID = "Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider"
)

type UserConfiguration struct {
	PlayDefaultAudioTrack      bool     `json:"PlayDefaultAudioTrack"`
	DisplayMissingEpisodes     bool     `json:"DisplayMissingEpisodes"`
	GroupedFolders             []string `json:"GroupedFolders"`
	SubtitleMode               string   `json:"SubtitleMode"`
	DisplayCollectionsView     bool     `json:"DisplayCollectionsView"`
	EnableLocalPassword        bool     `json:"EnableLocalPassword"`
	OrderedViews               []string `json:"OrderedViews"`
	LatestItemsExcludes        []string `json:"LatestItemsExcludes"`
	MyMediaExcludes            []string `json:"MyMediaExcludes"`
	HidePlayedInLatest         bool     `json:"HidePlayedInLatest"`
	RememberAudioSelections    bool     `json:"RememberAudioSelections"`
	RememberSubtitleSelections bool     `json:"RememberSubtitleSelections"`
	EnableNextEpisodeAutoPlay  bool     `json:"EnableNextEpisodeAutoPlay"`
}

type UserPolicy struct {
	IsAdministrator                bool     `json:"IsAdministrator"`
	IsHidden                       bool     `json:"IsHidden"`
	IsDisabled                     bool     `json:"IsDisabled"`
	EnableAllFolders               bool     `json:"EnableAllFolders"`
	EnabledFolders                 []string `json:"EnabledFolders"`
	EnableMediaPlayback            bool     `json:"EnableMediaPlayback"`
	EnableAudioPlaybackTranscoding bool     `json:"EnableAudioPlaybackTranscoding"`
	EnableVideoPlaybackTranscoding bool     `json:"EnableVideoPlaybackTranscoding"`
	EnablePlaybackRemuxing         bool     `json:"EnablePlaybackRemuxing"`
	EnableContentDownloading       bool     `json:"EnableContentDownloading"`
	EnablePublicSharing            bool     `json:"EnablePublicSharing"`
	AuthenticationProviderId       string   `json:"AuthenticationProviderId"`
	PasswordResetProviderId        string   `json:"PasswordResetProviderId"`
}

type UserDto struct {
	Name                      string            `json:"Name"`
	ServerId                  string            `json:"ServerId"`
	Id                        string            `json:"Id"`
	HasPassword               bool              `json:"HasPassword"`
	HasConfiguredPassword     bool              `json:"HasConfiguredPassword"`
	HasConfiguredEasyPassword bool              `json:"HasConfiguredEasyPassword"`
	EnableAutoLogin           bool              `json:"EnableAutoLogin"`
	Configuration             UserConfiguration `json:"Configuration"`
	Policy                    UserPolicy        `json:"Policy"`
}

type PlayStateDto struct {
	PositionTicks       int64  `json:"PositionTicks"`
	CanSeek             bool   `json:"CanSeek"`
	IsPaused            bool   `json:"IsPaused"`
	IsMuted             bool   `json:"IsMuted"`
	MediaSourceId       string `json:"MediaSourceId,omitempty"`
	PlayMethod          string `json:"PlayMethod,omitempty"`
	RepeatMode          string `json:"RepeatMode"`
	PlaybackOrder       string `json:"PlaybackOrder"`
}

type SessionCapabilitiesDto struct {
	PlayableMediaTypes           []string `json:"PlayableMediaTypes"`
	SupportedCommands            []string `json:"SupportedCommands"`
	SupportsMediaControl         bool     `json:"SupportsMediaControl"`
	SupportsPersistentIdentifier bool     `json:"SupportsPersistentIdentifier"`
}

type SessionInfoDto struct {
	PlayState                *PlayStateDto           `json:"PlayState"`
	AdditionalUsers          []any                   `json:"AdditionalUsers"`
	Capabilities             *SessionCapabilitiesDto `json:"Capabilities"`
	RemoteEndPoint           string                  `json:"RemoteEndPoint,omitempty"`
	PlayableMediaTypes       []string                `json:"PlayableMediaTypes"`
	Id                       string                  `json:"Id"`
	UserId                   string                  `json:"UserId"`
	UserName                 string                  `json:"UserName"`
	Client                   string                  `json:"Client"`
	LastActivityDate         string                  `json:"LastActivityDate"`
	LastPlaybackCheckIn      string                  `json:"LastPlaybackCheckIn"`
	DeviceName               string                  `json:"DeviceName"`
	DeviceId                 string                  `json:"DeviceId"`
	ApplicationVersion       string                  `json:"ApplicationVersion"`
	IsActive                 bool                    `json:"IsActive"`
	SupportsMediaControl     bool                    `json:"SupportsMediaControl"`
	SupportsRemoteControl    bool                    `json:"SupportsRemoteControl"`
	NowPlayingQueue          []any                   `json:"NowPlayingQueue"`
	NowPlayingQueueFullItems []any                   `json:"NowPlayingQueueFullItems"`
	HasCustomDeviceName      bool                    `json:"HasCustomDeviceName"`
	ServerId                 string                  `json:"ServerId"`
	SupportedCommands        []string                `json:"SupportedCommands"`
}

type AuthenticationResult struct {
	User        UserDto        `json:"User"`
	SessionInfo SessionInfoDto `json:"SessionInfo"`
	AccessToken string         `json:"AccessToken"`
	ServerId    string         `json:"ServerId"`
}

type DisplayPreferencesDto struct {
	Id                 string            `json:"Id"`
	ViewType           string            `json:"ViewType"`
	SortBy             string            `json:"SortBy"`
	IndexBy            string            `json:"IndexBy,omitempty"`
	RememberIndexing   bool              `json:"RememberIndexing"`
	PrimaryImageHeight int               `json:"PrimaryImageHeight"`
	PrimaryImageWidth  int               `json:"PrimaryImageWidth"`
	CustomPrefs        map[string]string `json:"CustomPrefs"`
	ScrollDirection    string            `json:"ScrollDirection"`
	ShowBackdrop       bool              `json:"ShowBackdrop"`
	RememberSorting    bool              `json:"RememberSorting"`
	SortOrder          string            `json:"SortOrder"`
	ShowSidebar        bool              `json:"ShowSidebar"`
	Client             string            `json:"Client,omitempty"`
}

type authenticateByNameRequest struct {
	Username string `json:"Username"`
	Pw       string `json:"Pw"`
	Password string `json:"Password"`
}

func toUserDto(u db.ProxyUser, serverID string) UserDto {
	return UserDto{
		Name:                      u.Username,
		ServerId:                  serverID,
		Id:                        u.ID,
		HasPassword:               true,
		HasConfiguredPassword:     true,
		HasConfiguredEasyPassword: false,
		EnableAutoLogin:           false,
		Configuration: UserConfiguration{
			PlayDefaultAudioTrack:      true,
			DisplayMissingEpisodes:     false,
			GroupedFolders:             []string{},
			SubtitleMode:               "Default",
			DisplayCollectionsView:     false,
			EnableLocalPassword:        false,
			OrderedViews:               []string{},
			LatestItemsExcludes:        []string{},
			MyMediaExcludes:            []string{},
			HidePlayedInLatest:         true,
			RememberAudioSelections:    true,
			RememberSubtitleSelections: true,
			EnableNextEpisodeAutoPlay:  true,
		},
		Policy: UserPolicy{
			IsAdministrator:                u.IsAdmin,
			IsHidden:                       false,
			IsDisabled:                     false,
			EnableAllFolders:               true,
			EnabledFolders:                 []string{},
			EnableMediaPlayback:            true,
			EnableAudioPlaybackTranscoding: true,
			EnableVideoPlaybackTranscoding: true,
			EnablePlaybackRemuxing:         true,
			EnableContentDownloading:       true,
			EnablePublicSharing:            false,
			AuthenticationProviderId:       defaultAuthProviderID,
			PasswordResetProviderId:        defaultPasswordResetProviderID,
		},
	}
}

func buildSessionInfo(user db.ProxyUser, sess db.ProxySession, serverID string, remoteAddr string) SessionInfoDto {
	now := time.Now().UTC().Format(time.RFC3339)
	if !sess.LastSeenAt.IsZero() {
		now = sess.LastSeenAt.UTC().Format(time.RFC3339)
	}
	return SessionInfoDto{
		PlayState: &PlayStateDto{
			PositionTicks: 0,
			CanSeek:       false,
			IsPaused:      false,
			IsMuted:       false,
			RepeatMode:    "RepeatNone",
			PlaybackOrder: "Default",
		},
		AdditionalUsers: []any{},
		Capabilities: &SessionCapabilitiesDto{
			PlayableMediaTypes:           []string{"Video", "Audio"},
			SupportedCommands:            []string{},
			SupportsMediaControl:         false,
			SupportsPersistentIdentifier: true,
		},
		RemoteEndPoint:           remoteAddr,
		PlayableMediaTypes:       []string{"Video", "Audio"},
		Id:                       idgen.DeterministicHex32("session:" + sess.Token),
		UserId:                   user.ID,
		UserName:                 user.Username,
		Client:                   sess.ClientName,
		LastActivityDate:         now,
		LastPlaybackCheckIn:      now,
		DeviceName:               sess.DeviceName,
		DeviceId:                 sess.DeviceID,
		ApplicationVersion:       sess.AppVersion,
		IsActive:                 true,
		SupportsMediaControl:     false,
		SupportsRemoteControl:    false,
		NowPlayingQueue:          []any{},
		NowPlayingQueueFullItems: []any{},
		HasCustomDeviceName:      false,
		ServerId:                 serverID,
		SupportedCommands:        []string{},
	}
}

func defaultDisplayPreferences(prefID, client string) DisplayPreferencesDto {
	return DisplayPreferencesDto{
		Id:              prefID,
		ViewType:        "posters",
		SortBy:          "SortName",
		CustomPrefs:     map[string]string{},
		ScrollDirection: "Horizontal",
		ShowBackdrop:    true,
		SortOrder:       "Ascending",
		Client:          client,
	}
}

func (h *apiHandler) handlePublicUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		h.logger.Error("failed to list public users", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	serverID, _ := h.serverIdentity(r.Context())
	out := make([]UserDto, 0, len(users))
	for _, u := range users {
		out = append(out, toUserDto(u, serverID))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *apiHandler) handleAuthenticateByName(w http.ResponseWriter, r *http.Request) {
	var req authenticateByNameRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	username := strings.TrimSpace(req.Username)
	pw := req.Pw
	if pw == "" {
		pw = req.Password
	}
	if username == "" {
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	user, err := h.db.GetUserByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		h.logger.Error("failed to look up user", "username", username, "error", err)
		writeError(w, http.StatusInternalServerError, "authentication error")
		return
	}

	if !auth.VerifyPassword(user.PasswordHash, pw) {
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	clientInfo := auth.ParseRequest(r)
	deviceID := strings.TrimSpace(clientInfo.DeviceID)
	if deviceID == "" {
		deviceID = idgen.NewRandomHex32()
	}

	accessToken := idgen.NewRandomHex32()
	sess := db.ProxySession{
		Token:      accessToken,
		UserID:     user.ID,
		DeviceID:   deviceID,
		ClientName: clientInfo.Client,
		DeviceName: clientInfo.Device,
		AppVersion: clientInfo.Version,
		LastSeenAt: time.Now().UTC(),
	}
	if err := h.db.UpsertSession(r.Context(), sess); err != nil {
		h.logger.Error("failed to persist proxy session", "user_id", user.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	serverID, _ := h.serverIdentity(r.Context())
	writeJSON(w, http.StatusOK, AuthenticationResult{
		User:        toUserDto(*user, serverID),
		SessionInfo: buildSessionInfo(*user, sess, serverID, r.RemoteAddr),
		AccessToken: accessToken,
		ServerId:    serverID,
	})
}

func (h *apiHandler) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		h.logger.Error("failed to list users", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	serverID, _ := h.serverIdentity(r.Context())
	out := make([]UserDto, 0, len(users))
	for _, u := range users {
		out = append(out, toUserDto(u, serverID))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *apiHandler) handleUserMe(w http.ResponseWriter, r *http.Request) {
	ac, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	serverID, _ := h.serverIdentity(r.Context())
	writeJSON(w, http.StatusOK, toUserDto(ac.User, serverID))
}

func (h *apiHandler) handleUserByID(w http.ResponseWriter, r *http.Request) {
	rawID := chi.URLParam(r, "userId")
	userID, ok := normalizeRouteID(rawID)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		h.logger.Error("failed to get user", "user_id", userID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get user")
		return
	}

	serverID, _ := h.serverIdentity(r.Context())
	writeJSON(w, http.StatusOK, toUserDto(*user, serverID))
}

func (h *apiHandler) resolveDisplayPrefsKey(r *http.Request, ac *auth.Context) (userID, prefID, client string, ok bool) {
	prefID = strings.TrimSpace(chi.URLParam(r, "displayPreferencesId"))
	userID = ac.User.ID

	rawUserID := strings.TrimSpace(r.URL.Query().Get("userId"))
	if rawUserID == "" {
		rawUserID = strings.TrimSpace(r.URL.Query().Get("UserId"))
	}
	if rawUserID != "" {
		normalized, valid := normalizeRouteID(rawUserID)
		if !valid {
			return "", "", "", false
		}
		userID = normalized
	}

	client = strings.TrimSpace(r.URL.Query().Get("client"))
	if client == "" {
		client = strings.TrimSpace(r.URL.Query().Get("Client"))
	}
	if client == "" {
		client = strings.TrimSpace(ac.Client.Client)
	}
	if client == "" {
		client = strings.TrimSpace(ac.Session.ClientName)
	}
	if client == "" {
		client = "emby"
	}
	return userID, prefID, client, true
}

func (h *apiHandler) handleGetDisplayPreferences(w http.ResponseWriter, r *http.Request) {
	ac, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	userID, prefID, client, valid := h.resolveDisplayPrefsKey(r, ac)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	dto := defaultDisplayPreferences(prefID, client)
	raw, err := h.db.GetDisplayPreferences(r.Context(), userID, prefID, client)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusOK, dto)
			return
		}
		h.logger.Error("failed to get display preferences", "user_id", userID, "pref_id", prefID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load display preferences")
		return
	}

	if err := json.Unmarshal([]byte(raw), &dto); err != nil {
		writeJSON(w, http.StatusOK, defaultDisplayPreferences(prefID, client))
		return
	}
	if dto.Id == "" {
		dto.Id = prefID
	}
	if dto.Client == "" {
		dto.Client = client
	}
	if dto.CustomPrefs == nil {
		dto.CustomPrefs = map[string]string{}
	}
	if dto.ViewType == "" {
		dto.ViewType = "posters"
	}
	if dto.SortBy == "" {
		dto.SortBy = "SortName"
	}
	if dto.SortOrder == "" {
		dto.SortOrder = "Ascending"
	}
	if dto.ScrollDirection == "" {
		dto.ScrollDirection = "Horizontal"
	}
	writeJSON(w, http.StatusOK, dto)
}

func (h *apiHandler) handlePostDisplayPreferences(w http.ResponseWriter, r *http.Request) {
	ac, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	userID, prefID, client, valid := h.resolveDisplayPrefsKey(r, ac)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	dto := defaultDisplayPreferences(prefID, client)
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &dto); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON request body")
			return
		}
	}
	if dto.Id == "" {
		dto.Id = prefID
	}
	if dto.Client == "" {
		dto.Client = client
	}
	if dto.CustomPrefs == nil {
		dto.CustomPrefs = map[string]string{}
	}

	encoded, err := json.Marshal(dto)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode display preferences")
		return
	}

	if err := h.db.UpsertDisplayPreferences(r.Context(), userID, prefID, client, string(encoded)); err != nil {
		h.logger.Error("failed to save display preferences", "user_id", userID, "pref_id", prefID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save display preferences")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandler) handleNoContent(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	ac, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	if err := h.db.DeleteSession(r.Context(), ac.Session.Token); err != nil {
		h.logger.Error("failed to delete session on logout", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to logout")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandler) handleSessions(w http.ResponseWriter, r *http.Request) {
	ac, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	serverID, _ := h.serverIdentity(r.Context())
	writeJSON(w, http.StatusOK, []SessionInfoDto{
		buildSessionInfo(ac.User, ac.Session, serverID, r.RemoteAddr),
	})
}
