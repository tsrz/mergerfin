package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/tsrz/mergerfin/internal/idgen"
)

// Domain structs persisted in SQLite.

type ProxyUser struct {
	ID           string
	Username     string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time
}

type ProxySession struct {
	Token      string
	UserID     string
	DeviceID   string
	ClientName string
	DeviceName string
	AppVersion string
	LastSeenAt time.Time
}

type UpstreamServer struct {
	ID            string
	Name          string
	BaseURL       string
	Username      string
	RemoteUserID  string
	AccessToken   string
	Priority      int
	StreamMode    string
	Enabled       bool
	ServerVersion string
	IsOnline      bool
	LatencyMs     int64
	LastSyncAt    *time.Time
	LastError     string
}

type VirtualLibrary struct {
	ID             string
	Name           string
	CollectionType string
}

type VirtualItem struct {
	ID                  string
	LibraryID           string
	ParentID            string
	SeriesID            string
	SeasonID            string
	ItemType            string
	CanonicalKey        string
	Name                string
	SortName            string
	OriginalTitle       string
	Overview            string
	Tagline             string
	ProductionYear      int
	PremiereDate        string
	CommunityRating     float64
	OfficialRating      string
	IndexNumber         int
	ParentIndexNumber   int
	RunTimeTicks        int64
	SeriesName          string
	SeasonName          string
	GenresJSON          string
	StudiosJSON         string
	PeopleJSON          string
	ProviderIDsJSON     string
	ImgPrimaryServerID  string
	ImgPrimaryRemoteID  string
	ImgBackdropServerID string
	ImgBackdropRemoteID string
	DateCreated         string
	UpdatedAt           time.Time
}

type ItemFilter struct {
	IDs          []string
	UserID       string
	ParentID     string
	LibraryID    string
	SeriesID     string
	SeasonID     string
	IncludeTypes []string
	ExcludeTypes []string
	Recursive    bool
	SearchTerm   string
	Genres       []string
	Years        []int
	IsPlayed     *bool
	IsFavorite   *bool
	SortBy       string
	SortOrder    string
	StartIndex   int
	Limit        int
}

type ItemFilters struct {
	Genres          []string
	Years           []int
	OfficialRatings []string
}

type ItemSource struct {
	SourceID             string
	VirtualItemID        string
	ServerID             string
	RemoteItemID         string
	RemoteMediaSourceID  string
	Label                string
	Container            string
	VideoCodec           string
	AudioCodec           string
	Width                int
	Height               int
	Bitrate              int64
	SizeBytes            int64
	RunTimeTicks         int64
	RawMediaStreamsJSON  string
	RawMediaSourceJSON   string
}

type UserItemData struct {
	UserID                string
	VirtualItemID         string
	Played                bool
	PlaybackPositionTicks int64
	PlayCount             int
	IsFavorite            bool
	LastPlayedAt          string
}

type ActivePlaySession struct {
	PlaySessionID          string
	UserID                 string
	DeviceID               string
	VirtualItemID          string
	SourceID               string
	ServerID               string
	RemoteItemID           string
	RemotePlaySessionID    string
	UpstreamTranscodingURL string
	StartedAt              time.Time
}

type rowScanner interface {
	Scan(dest ...any) error
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func defaultJSON(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return val
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// --- Settings ---

func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var val string
	err := d.sqlDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err != nil {
		return "", err
	}
	return val, nil
}

func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

// --- ProxyUsers ---

func scanProxyUser(s rowScanner) (ProxyUser, error) {
	var u ProxyUser
	var isAdmin int
	err := s.Scan(&u.ID, &u.Username, &u.PasswordHash, &isAdmin, &u.CreatedAt)
	u.IsAdmin = isAdmin != 0
	return u, err
}

func (d *DB) CreateUser(ctx context.Context, u ProxyUser) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO proxy_users(id, username, password_hash, is_admin)
		VALUES(?, ?, ?, ?)
	`, u.ID, u.Username, u.PasswordHash, boolToInt(u.IsAdmin))
	return err
}

func (d *DB) GetUserByID(ctx context.Context, id string) (*ProxyUser, error) {
	u, err := scanProxyUser(d.sqlDB.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_admin, created_at
		FROM proxy_users WHERE id = ?
	`, id))
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) GetUserByUsername(ctx context.Context, username string) (*ProxyUser, error) {
	u, err := scanProxyUser(d.sqlDB.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_admin, created_at
		FROM proxy_users WHERE username = ?
	`, username))
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) ListUsers(ctx context.Context) ([]ProxyUser, error) {
	rows, err := d.sqlDB.QueryContext(ctx, `
		SELECT id, username, password_hash, is_admin, created_at
		FROM proxy_users ORDER BY created_at ASC, username ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProxyUser
	for rows.Next() {
		u, err := scanProxyUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (d *DB) DeleteUser(ctx context.Context, id string) error {
	_, err := d.sqlDB.ExecContext(ctx, `DELETE FROM proxy_users WHERE id = ?`, id)
	return err
}

func (d *DB) UpdateUserPassword(ctx context.Context, id, passwordHash string) error {
	res, err := d.sqlDB.ExecContext(ctx, `UPDATE proxy_users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := d.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_users`).Scan(&count)
	return count, err
}

// --- ProxySessions ---

func (d *DB) UpsertSession(ctx context.Context, s ProxySession) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO proxy_sessions(token, user_id, device_id, client_name, device_name, app_version, last_seen_at)
		VALUES(?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(token) DO UPDATE SET
			user_id = excluded.user_id,
			device_id = excluded.device_id,
			client_name = excluded.client_name,
			device_name = excluded.device_name,
			app_version = excluded.app_version,
			last_seen_at = CURRENT_TIMESTAMP
	`, s.Token, s.UserID, s.DeviceID, s.ClientName, s.DeviceName, s.AppVersion)
	return err
}

func (d *DB) GetSessionByToken(ctx context.Context, token string) (*ProxySession, error) {
	var s ProxySession
	err := d.sqlDB.QueryRowContext(ctx, `
		SELECT token, user_id, device_id, client_name, device_name, app_version, last_seen_at
		FROM proxy_sessions WHERE token = ?
	`, token).Scan(&s.Token, &s.UserID, &s.DeviceID, &s.ClientName, &s.DeviceName, &s.AppVersion, &s.LastSeenAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (d *DB) TouchSession(ctx context.Context, token string) error {
	_, err := d.sqlDB.ExecContext(ctx, `UPDATE proxy_sessions SET last_seen_at = CURRENT_TIMESTAMP WHERE token = ?`, token)
	return err
}

func (d *DB) DeleteSession(ctx context.Context, token string) error {
	_, err := d.sqlDB.ExecContext(ctx, `DELETE FROM proxy_sessions WHERE token = ?`, token)
	return err
}

// --- DisplayPreferences ---

func (d *DB) GetDisplayPreferences(ctx context.Context, userID, prefID, client string) (string, error) {
	var payload string
	err := d.sqlDB.QueryRowContext(ctx, `
		SELECT json_payload FROM display_preferences
		WHERE user_id = ? AND pref_id = ? AND client = ?
	`, userID, prefID, client).Scan(&payload)
	if err != nil {
		return "", err
	}
	return payload, nil
}

func (d *DB) UpsertDisplayPreferences(ctx context.Context, userID, prefID, client, jsonPayload string) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO display_preferences(user_id, pref_id, client, json_payload)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(user_id, pref_id, client) DO UPDATE SET
			json_payload = excluded.json_payload
	`, userID, prefID, client, defaultJSON(jsonPayload, "{}"))
	return err
}

// --- UpstreamServers ---

const upstreamSelectCols = `
	id, name, base_url, username, remote_user_id, access_token,
	priority, stream_mode, enabled, server_version, is_online, latency_ms,
	last_sync_at, last_error
`

func scanUpstreamServer(s rowScanner) (UpstreamServer, error) {
	var srv UpstreamServer
	var enabled, isOnline int
	var lastSync sql.NullTime
	err := s.Scan(
		&srv.ID, &srv.Name, &srv.BaseURL, &srv.Username, &srv.RemoteUserID, &srv.AccessToken,
		&srv.Priority, &srv.StreamMode, &enabled, &srv.ServerVersion, &isOnline, &srv.LatencyMs,
		&lastSync, &srv.LastError,
	)
	if err != nil {
		return srv, err
	}
	srv.Enabled = enabled != 0
	srv.IsOnline = isOnline != 0
	if lastSync.Valid {
		t := lastSync.Time
		srv.LastSyncAt = &t
	}
	return srv, nil
}

func (d *DB) UpsertServer(ctx context.Context, s UpstreamServer) error {
	streamMode := s.StreamMode
	if streamMode == "" {
		streamMode = "proxy"
	}
	priority := s.Priority
	if priority == 0 {
		priority = 100
	}
	var lastSync any
	if s.LastSyncAt != nil {
		lastSync = *s.LastSyncAt
	}
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO upstream_servers(
			id, name, base_url, username, remote_user_id, access_token,
			priority, stream_mode, enabled, server_version, is_online, latency_ms,
			last_sync_at, last_error
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			base_url = excluded.base_url,
			username = excluded.username,
			remote_user_id = excluded.remote_user_id,
			access_token = excluded.access_token,
			priority = excluded.priority,
			stream_mode = excluded.stream_mode,
			enabled = excluded.enabled,
			server_version = excluded.server_version,
			is_online = excluded.is_online,
			latency_ms = excluded.latency_ms,
			last_sync_at = excluded.last_sync_at,
			last_error = excluded.last_error
	`, s.ID, s.Name, s.BaseURL, s.Username, s.RemoteUserID, s.AccessToken,
		priority, streamMode, boolToInt(s.Enabled), s.ServerVersion, boolToInt(s.IsOnline), s.LatencyMs,
		lastSync, s.LastError)
	return err
}

func (d *DB) UpdateServer(ctx context.Context, s UpstreamServer) error {
	streamMode := s.StreamMode
	if streamMode == "" {
		streamMode = "proxy"
	}
	res, err := d.sqlDB.ExecContext(ctx, `
		UPDATE upstream_servers SET
			name = ?, base_url = ?, username = ?, remote_user_id = ?, access_token = ?,
			priority = ?, stream_mode = ?, enabled = ?
		WHERE id = ?
	`, s.Name, s.BaseURL, s.Username, s.RemoteUserID, s.AccessToken,
		s.Priority, streamMode, boolToInt(s.Enabled), s.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

func (d *DB) DeleteServer(ctx context.Context, id string) error {
	_, err := d.sqlDB.ExecContext(ctx, `DELETE FROM upstream_servers WHERE id = ?`, id)
	return err
}

func (d *DB) GetServerByID(ctx context.Context, id string) (*UpstreamServer, error) {
	srv, err := scanUpstreamServer(d.sqlDB.QueryRowContext(ctx, `SELECT `+upstreamSelectCols+` FROM upstream_servers WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &srv, nil
}

func (d *DB) listServersWhere(ctx context.Context, where string) ([]UpstreamServer, error) {
	q := `SELECT ` + upstreamSelectCols + ` FROM upstream_servers ` + where + ` ORDER BY priority ASC, name ASC`
	rows, err := d.sqlDB.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UpstreamServer
	for rows.Next() {
		srv, err := scanUpstreamServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

func (d *DB) ListServers(ctx context.Context) ([]UpstreamServer, error) {
	return d.listServersWhere(ctx, "")
}

func (d *DB) ListEnabledServers(ctx context.Context) ([]UpstreamServer, error) {
	return d.listServersWhere(ctx, "WHERE enabled = 1")
}

func (d *DB) UpdateServerStatus(ctx context.Context, id string, isOnline bool, latencyMs int64, serverVersion string, lastErr string) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		UPDATE upstream_servers
		SET is_online = ?, latency_ms = ?, server_version = ?, last_error = ?
		WHERE id = ?
	`, boolToInt(isOnline), latencyMs, serverVersion, lastErr, id)
	return err
}

func (d *DB) UpdateServerSyncState(ctx context.Context, id string, lastSyncAt time.Time, lastErr string) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		UPDATE upstream_servers
		SET last_sync_at = ?, last_error = ?
		WHERE id = ?
	`, lastSyncAt.UTC(), lastErr, id)
	return err
}

// --- VirtualLibraries ---

func (d *DB) ListLibraries(ctx context.Context) ([]VirtualLibrary, error) {
	rows, err := d.sqlDB.QueryContext(ctx, `SELECT id, name, collection_type FROM virtual_libraries ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VirtualLibrary
	for rows.Next() {
		var lib VirtualLibrary
		if err := rows.Scan(&lib.ID, &lib.Name, &lib.CollectionType); err != nil {
			return nil, err
		}
		out = append(out, lib)
	}
	return out, rows.Err()
}

func (d *DB) GetLibraryByID(ctx context.Context, id string) (*VirtualLibrary, error) {
	var lib VirtualLibrary
	err := d.sqlDB.QueryRowContext(ctx, `SELECT id, name, collection_type FROM virtual_libraries WHERE id = ?`, id).
		Scan(&lib.ID, &lib.Name, &lib.CollectionType)
	if err != nil {
		return nil, err
	}
	return &lib, nil
}

// --- VirtualItems ---

const itemSelectCols = `
	vi.id, vi.library_id, vi.parent_id, vi.series_id, vi.season_id, vi.item_type, vi.canonical_key,
	vi.name, vi.sort_name, vi.original_title, vi.overview, vi.tagline,
	vi.production_year, vi.premiere_date, vi.community_rating, vi.official_rating,
	vi.index_number, vi.parent_index_number, vi.run_time_ticks, vi.series_name, vi.season_name,
	vi.genres_json, vi.studios_json, vi.people_json, vi.provider_ids_json,
	vi.img_primary_server_id, vi.img_primary_remote_id, vi.img_backdrop_server_id, vi.img_backdrop_remote_id,
	vi.date_created, vi.updated_at
`

func scanVirtualItem(s rowScanner) (VirtualItem, error) {
	var it VirtualItem
	err := s.Scan(
		&it.ID, &it.LibraryID, &it.ParentID, &it.SeriesID, &it.SeasonID, &it.ItemType, &it.CanonicalKey,
		&it.Name, &it.SortName, &it.OriginalTitle, &it.Overview, &it.Tagline,
		&it.ProductionYear, &it.PremiereDate, &it.CommunityRating, &it.OfficialRating,
		&it.IndexNumber, &it.ParentIndexNumber, &it.RunTimeTicks, &it.SeriesName, &it.SeasonName,
		&it.GenresJSON, &it.StudiosJSON, &it.PeopleJSON, &it.ProviderIDsJSON,
		&it.ImgPrimaryServerID, &it.ImgPrimaryRemoteID, &it.ImgBackdropServerID, &it.ImgBackdropRemoteID,
		&it.DateCreated, &it.UpdatedAt,
	)
	return it, err
}

func scanVirtualItems(rows *sql.Rows) ([]VirtualItem, error) {
	var out []VirtualItem
	for rows.Next() {
		it, err := scanVirtualItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (d *DB) UpsertItem(ctx context.Context, item VirtualItem) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO virtual_items(
			id, library_id, parent_id, series_id, season_id, item_type, canonical_key,
			name, sort_name, original_title, overview, tagline,
			production_year, premiere_date, community_rating, official_rating,
			index_number, parent_index_number, run_time_ticks, series_name, season_name,
			genres_json, studios_json, people_json, provider_ids_json,
			img_primary_server_id, img_primary_remote_id, img_backdrop_server_id, img_backdrop_remote_id,
			date_created, updated_at
		) VALUES(
			?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, CURRENT_TIMESTAMP
		)
		ON CONFLICT(id) DO UPDATE SET
			library_id = excluded.library_id,
			parent_id = excluded.parent_id,
			series_id = excluded.series_id,
			season_id = excluded.season_id,
			item_type = excluded.item_type,
			canonical_key = excluded.canonical_key,
			name = excluded.name,
			sort_name = excluded.sort_name,
			original_title = excluded.original_title,
			overview = excluded.overview,
			tagline = excluded.tagline,
			production_year = excluded.production_year,
			premiere_date = excluded.premiere_date,
			community_rating = excluded.community_rating,
			official_rating = excluded.official_rating,
			index_number = excluded.index_number,
			parent_index_number = excluded.parent_index_number,
			run_time_ticks = excluded.run_time_ticks,
			series_name = excluded.series_name,
			season_name = excluded.season_name,
			genres_json = excluded.genres_json,
			studios_json = excluded.studios_json,
			people_json = excluded.people_json,
			provider_ids_json = excluded.provider_ids_json,
			img_primary_server_id = excluded.img_primary_server_id,
			img_primary_remote_id = excluded.img_primary_remote_id,
			img_backdrop_server_id = excluded.img_backdrop_server_id,
			img_backdrop_remote_id = excluded.img_backdrop_remote_id,
			date_created = excluded.date_created,
			updated_at = CURRENT_TIMESTAMP
	`,
		item.ID, item.LibraryID, item.ParentID, item.SeriesID, item.SeasonID, item.ItemType, item.CanonicalKey,
		item.Name, item.SortName, item.OriginalTitle, item.Overview, item.Tagline,
		item.ProductionYear, item.PremiereDate, item.CommunityRating, item.OfficialRating,
		item.IndexNumber, item.ParentIndexNumber, item.RunTimeTicks, item.SeriesName, item.SeasonName,
		defaultJSON(item.GenresJSON, "[]"), defaultJSON(item.StudiosJSON, "[]"), defaultJSON(item.PeopleJSON, "[]"), defaultJSON(item.ProviderIDsJSON, "{}"),
		item.ImgPrimaryServerID, item.ImgPrimaryRemoteID, item.ImgBackdropServerID, item.ImgBackdropRemoteID,
		item.DateCreated,
	)
	return err
}

func (d *DB) GetItemByID(ctx context.Context, id string) (*VirtualItem, error) {
	it, err := scanVirtualItem(d.sqlDB.QueryRowContext(ctx, `SELECT `+itemSelectCols+` FROM virtual_items vi WHERE vi.id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// sanitizeFTSQuery turns arbitrary user input into a safe FTS5 MATCH expression.
// Each whitespace-delimited token containing at least one letter or digit is
// double-quoted (preventing FTS5 syntax errors on -, +, *, :, ^, (), AND/OR/NOT)
// with a prefix wildcard `*` so partial words match cleanly.
func sanitizeFTSQuery(raw string) (string, bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "", false
	}

	var tokens []string
	for _, field := range fields {
		hasAlnum := false
		for _, r := range field {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				hasAlnum = true
				break
			}
		}
		if !hasAlnum {
			continue
		}
		escaped := strings.ReplaceAll(field, `"`, `""`)
		tokens = append(tokens, `"`+escaped+`"*`)
	}
	if len(tokens) == 0 {
		return "", false
	}
	return strings.Join(tokens, " "), true
}

func (d *DB) QueryItems(ctx context.Context, f ItemFilter) ([]VirtualItem, int, error) {
	var joins []string
	var where []string
	var args []any

	needUserJoin := f.IsPlayed != nil || f.IsFavorite != nil || strings.EqualFold(f.SortBy, "DatePlayed")
	if needUserJoin {
		joins = append(joins, `LEFT JOIN user_item_data uid ON uid.virtual_item_id = vi.id AND uid.user_id = ?`)
		args = append(args, f.UserID)
	}

	if len(f.IDs) > 0 {
		where = append(where, `vi.id IN (`+placeholders(len(f.IDs))+`)`)
		for _, id := range f.IDs {
			if norm, err := idgen.NormalizeID(id); err == nil {
				args = append(args, norm)
			} else {
				args = append(args, strings.TrimSpace(id))
			}
		}
	}
	if f.ParentID != "" {
		if f.Recursive {
			where = append(where, `(vi.library_id = ? OR vi.parent_id = ? OR vi.series_id = ? OR vi.season_id = ?)`)
			args = append(args, f.ParentID, f.ParentID, f.ParentID, f.ParentID)
		} else {
			where = append(where, `vi.parent_id = ?`)
			args = append(args, f.ParentID)
		}
	}
	if f.LibraryID != "" {
		where = append(where, `vi.library_id = ?`)
		args = append(args, f.LibraryID)
	}
	if f.SeriesID != "" {
		where = append(where, `vi.series_id = ?`)
		args = append(args, f.SeriesID)
	}
	if f.SeasonID != "" {
		where = append(where, `vi.season_id = ?`)
		args = append(args, f.SeasonID)
	}
	if len(f.IncludeTypes) > 0 {
		where = append(where, `vi.item_type IN (`+placeholders(len(f.IncludeTypes))+`)`)
		for _, t := range f.IncludeTypes {
			args = append(args, t)
		}
	}
	if len(f.ExcludeTypes) > 0 {
		where = append(where, `vi.item_type NOT IN (`+placeholders(len(f.ExcludeTypes))+`)`)
		for _, t := range f.ExcludeTypes {
			args = append(args, t)
		}
	}
	if strings.TrimSpace(f.SearchTerm) != "" {
		ftsQuery, ok := sanitizeFTSQuery(f.SearchTerm)
		if !ok {
			where = append(where, `1 = 0`)
		} else {
			where = append(where, `vi.id IN (SELECT item_id FROM virtual_items_fts WHERE virtual_items_fts MATCH ?)`)
			args = append(args, ftsQuery)
		}
	}
	if len(f.Genres) > 0 {
		where = append(where, `EXISTS (SELECT 1 FROM json_each(vi.genres_json) WHERE lower(json_each.value) IN (`+placeholders(len(f.Genres))+`))`)
		for _, g := range f.Genres {
			args = append(args, strings.ToLower(g))
		}
	}
	if len(f.Years) > 0 {
		where = append(where, `vi.production_year IN (`+placeholders(len(f.Years))+`)`)
		for _, y := range f.Years {
			args = append(args, y)
		}
	}
	if f.IsPlayed != nil {
		where = append(where, `COALESCE(uid.played, 0) = ?`)
		args = append(args, boolToInt(*f.IsPlayed))
	}
	if f.IsFavorite != nil {
		where = append(where, `COALESCE(uid.is_favorite, 0) = ?`)
		args = append(args, boolToInt(*f.IsFavorite))
	}

	fromClause := `FROM virtual_items vi`
	if len(joins) > 0 {
		fromClause += " " + strings.Join(joins, " ")
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	countSQL := `SELECT COUNT(*) ` + fromClause + whereClause
	if err := d.sqlDB.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count items: %w", err)
	}

	orderDir := "ASC"
	if strings.EqualFold(f.SortOrder, "Descending") || strings.EqualFold(f.SortOrder, "DESC") {
		orderDir = "DESC"
	}
	var orderBy string
	switch strings.ToLower(strings.TrimSpace(f.SortBy)) {
	case "datecreated":
		orderBy = `vi.date_created ` + orderDir + `, vi.sort_name COLLATE NOCASE ASC`
	case "premieredate":
		orderBy = `vi.premiere_date ` + orderDir + `, vi.sort_name COLLATE NOCASE ASC`
	case "productionyear":
		orderBy = `vi.production_year ` + orderDir + `, vi.sort_name COLLATE NOCASE ASC`
	case "communityrating":
		orderBy = `vi.community_rating ` + orderDir + `, vi.sort_name COLLATE NOCASE ASC`
	case "indexnumber":
		orderBy = `vi.parent_index_number ` + orderDir + `, vi.index_number ` + orderDir
	case "dateplayed":
		orderBy = `COALESCE(uid.last_played_at, '') ` + orderDir
	case "random":
		orderBy = `RANDOM()`
	default:
		orderBy = `COALESCE(NULLIF(vi.sort_name, ''), vi.name) COLLATE NOCASE ` + orderDir
	}

	querySQL := `SELECT ` + itemSelectCols + ` ` + fromClause + whereClause + ` ORDER BY ` + orderBy
	queryArgs := make([]any, len(args))
	copy(queryArgs, args)

	if f.Limit > 0 {
		querySQL += ` LIMIT ? OFFSET ?`
		offset := f.StartIndex
		if offset < 0 {
			offset = 0
		}
		queryArgs = append(queryArgs, f.Limit, offset)
	} else if f.StartIndex > 0 {
		querySQL += ` LIMIT -1 OFFSET ?`
		queryArgs = append(queryArgs, f.StartIndex)
	}

	rows, err := d.sqlDB.QueryContext(ctx, querySQL, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query items: %w", err)
	}
	defer rows.Close()

	items, err := scanVirtualItems(rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (d *DB) GetLatestItems(ctx context.Context, userID, parentID string, includeTypes []string, limit int) ([]VirtualItem, error) {
	if limit <= 0 {
		limit = 20
	}
	var where []string
	var args []any

	if parentID != "" {
		where = append(where, `(vi.library_id = ? OR vi.parent_id = ? OR vi.series_id = ?)`)
		args = append(args, parentID, parentID, parentID)
	}
	if len(includeTypes) > 0 {
		where = append(where, `vi.item_type IN (`+placeholders(len(includeTypes))+`)`)
		for _, t := range includeTypes {
			args = append(args, t)
		}
	}
	if userID != "" {
		where = append(where, `COALESCE(uid.played, 0) = 0`)
	}

	sqlStr := `SELECT ` + itemSelectCols + ` FROM virtual_items vi`
	if userID != "" {
		sqlStr += ` LEFT JOIN user_item_data uid ON uid.virtual_item_id = vi.id AND uid.user_id = ?`
		args = append([]any{userID}, args...)
	}
	if len(where) > 0 {
		sqlStr += ` WHERE ` + strings.Join(where, " AND ")
	}
	sqlStr += ` ORDER BY vi.date_created DESC, vi.updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.sqlDB.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVirtualItems(rows)
}

func (d *DB) GetResumeItems(ctx context.Context, userID, parentID string, includeTypes []string, limit int) ([]VirtualItem, error) {
	if limit <= 0 {
		limit = 20
	}
	where := []string{
		`uid.user_id = ?`,
		`uid.playback_position_ticks > 0`,
		`uid.played = 0`,
	}
	args := []any{userID}

	if parentID != "" {
		where = append(where, `(vi.library_id = ? OR vi.parent_id = ? OR vi.series_id = ?)`)
		args = append(args, parentID, parentID, parentID)
	}
	if len(includeTypes) > 0 {
		where = append(where, `vi.item_type IN (`+placeholders(len(includeTypes))+`)`)
		for _, t := range includeTypes {
			args = append(args, t)
		}
	}
	args = append(args, limit)

	sqlStr := `SELECT ` + itemSelectCols + `
		FROM virtual_items vi
		INNER JOIN user_item_data uid ON uid.virtual_item_id = vi.id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY uid.last_played_at DESC, vi.updated_at DESC
		LIMIT ?`

	rows, err := d.sqlDB.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVirtualItems(rows)
}

func (d *DB) GetFilters(ctx context.Context, parentID string, includeTypes []string) (ItemFilters, error) {
	var filters ItemFilters
	var where []string
	var args []any

	if parentID != "" {
		where = append(where, `(vi.library_id = ? OR vi.parent_id = ? OR vi.series_id = ?)`)
		args = append(args, parentID, parentID, parentID)
	}
	if len(includeTypes) > 0 {
		where = append(where, `vi.item_type IN (`+placeholders(len(includeTypes))+`)`)
		for _, t := range includeTypes {
			args = append(args, t)
		}
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	genreRows, err := d.sqlDB.QueryContext(ctx, `
		SELECT DISTINCT j.value
		FROM virtual_items vi, json_each(vi.genres_json) j
		`+whereSQL+`
		ORDER BY j.value COLLATE NOCASE ASC
	`, args...)
	if err != nil {
		return filters, err
	}
	defer genreRows.Close()
	for genreRows.Next() {
		var g string
		if err := genreRows.Scan(&g); err != nil {
			return filters, err
		}
		if g != "" {
			filters.Genres = append(filters.Genres, g)
		}
	}
	if err := genreRows.Err(); err != nil {
		return filters, err
	}

	yearWhere := append([]string(nil), where...)
	yearWhere = append(yearWhere, `vi.production_year > 0`)
	yearRows, err := d.sqlDB.QueryContext(ctx, `
		SELECT DISTINCT vi.production_year
		FROM virtual_items vi
		WHERE `+strings.Join(yearWhere, " AND ")+`
		ORDER BY vi.production_year DESC
	`, args...)
	if err != nil {
		return filters, err
	}
	defer yearRows.Close()
	for yearRows.Next() {
		var y int
		if err := yearRows.Scan(&y); err != nil {
			return filters, err
		}
		filters.Years = append(filters.Years, y)
	}
	if err := yearRows.Err(); err != nil {
		return filters, err
	}

	ratingWhere := append([]string(nil), where...)
	ratingWhere = append(ratingWhere, `vi.official_rating != ''`)
	ratingRows, err := d.sqlDB.QueryContext(ctx, `
		SELECT DISTINCT vi.official_rating
		FROM virtual_items vi
		WHERE `+strings.Join(ratingWhere, " AND ")+`
		ORDER BY vi.official_rating ASC
	`, args...)
	if err != nil {
		return filters, err
	}
	defer ratingRows.Close()
	for ratingRows.Next() {
		var r string
		if err := ratingRows.Scan(&r); err != nil {
			return filters, err
		}
		filters.OfficialRatings = append(filters.OfficialRatings, r)
	}
	return filters, ratingRows.Err()
}

func (d *DB) GetSimilarItems(ctx context.Context, itemID string, limit int) ([]VirtualItem, error) {
	if limit <= 0 {
		limit = 12
	}
	target, err := d.GetItemByID(ctx, itemID)
	if err != nil {
		return nil, err
	}

	rows, err := d.sqlDB.QueryContext(ctx, `
		SELECT `+itemSelectCols+`
		FROM virtual_items vi
		WHERE vi.item_type = ? AND vi.id != ?
		ORDER BY (
			SELECT COUNT(*)
			FROM json_each(vi.genres_json) g1
			INNER JOIN json_each(?) g2 ON lower(g1.value) = lower(g2.value)
		) DESC, vi.community_rating DESC, vi.sort_name COLLATE NOCASE ASC
		LIMIT ?
	`, target.ItemType, target.ID, defaultJSON(target.GenresJSON, "[]"), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVirtualItems(rows)
}

func (d *DB) GetSeasons(ctx context.Context, seriesID string) ([]VirtualItem, error) {
	rows, err := d.sqlDB.QueryContext(ctx, `
		SELECT `+itemSelectCols+`
		FROM virtual_items vi
		WHERE vi.item_type = 'Season' AND (vi.series_id = ? OR vi.parent_id = ?)
		ORDER BY vi.index_number ASC, vi.sort_name COLLATE NOCASE ASC
	`, seriesID, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVirtualItems(rows)
}

func (d *DB) GetEpisodes(ctx context.Context, seriesID, seasonID string) ([]VirtualItem, error) {
	where := []string{`vi.item_type = 'Episode'`}
	var args []any
	if seriesID != "" {
		where = append(where, `vi.series_id = ?`)
		args = append(args, seriesID)
	}
	if seasonID != "" {
		where = append(where, `(vi.season_id = ? OR vi.parent_id = ?)`)
		args = append(args, seasonID, seasonID)
	}

	rows, err := d.sqlDB.QueryContext(ctx, `
		SELECT `+itemSelectCols+`
		FROM virtual_items vi
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY vi.parent_index_number ASC, vi.index_number ASC, vi.sort_name COLLATE NOCASE ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVirtualItems(rows)
}

func (d *DB) GetNextUpEpisodes(ctx context.Context, userID, seriesID string, limit int) ([]VirtualItem, error) {
	if limit <= 0 {
		limit = 20
	}

	var seriesFilter string
	if seriesID != "" {
		seriesFilter = `AND vi.series_id = ?`
	}

	// For each series with watched/in-progress history (or the requested seriesID),
	// pick the earliest unplayed episode strictly after the furthest watched episode
	// (or the earliest unplayed episode if none is watched yet for a requested seriesID).
	requireHistory := `AND lp.series_id IS NOT NULL`
	if seriesID != "" {
		requireHistory = ``
	}

	query := `
		WITH last_played AS (
			SELECT
				e.series_id,
				MAX(e.parent_index_number * 100000 + e.index_number) AS max_pos,
				MAX(COALESCE(uid.last_played_at, '')) AS last_at
			FROM virtual_items e
			INNER JOIN user_item_data uid ON uid.virtual_item_id = e.id AND uid.user_id = ?
			WHERE e.item_type = 'Episode' AND e.series_id != '' AND (uid.played = 1 OR uid.playback_position_ticks > 0)
			GROUP BY e.series_id
		),
		candidates AS (
			SELECT
				vi.id,
				COALESCE(lp.last_at, '') AS last_at,
				ROW_NUMBER() OVER (
					PARTITION BY vi.series_id
					ORDER BY vi.parent_index_number ASC, vi.index_number ASC, vi.sort_name COLLATE NOCASE ASC
				) AS rn
			FROM virtual_items vi
			LEFT JOIN last_played lp ON lp.series_id = vi.series_id
			LEFT JOIN user_item_data uid ON uid.virtual_item_id = vi.id AND uid.user_id = ?
			WHERE vi.item_type = 'Episode'
				AND vi.series_id != ''
				` + seriesFilter + `
				` + requireHistory + `
				AND COALESCE(uid.played, 0) = 0
				AND (
					lp.max_pos IS NULL
					OR (vi.parent_index_number * 100000 + vi.index_number) > lp.max_pos
					OR uid.playback_position_ticks > 0
				)
		)
		SELECT ` + itemSelectCols + `
		FROM virtual_items vi
		INNER JOIN candidates c ON c.id = vi.id
		WHERE c.rn = 1
		ORDER BY c.last_at DESC, vi.series_name COLLATE NOCASE ASC
		LIMIT ?
	`
	// Reorder args to match placeholder order: userID (CTE), userID (JOIN), [seriesID], limit.
	orderedArgs := []any{userID, userID}
	if seriesID != "" {
		orderedArgs = append(orderedArgs, seriesID)
	}
	orderedArgs = append(orderedArgs, limit)

	rows, err := d.sqlDB.QueryContext(ctx, query, orderedArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVirtualItems(rows)
}

func (d *DB) DeleteOrphanItems(ctx context.Context) (int64, error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var totalDeleted int64

	// 1. Delete non-container items (Movie, Episode, etc.) that have no sources.
	res, err := tx.ExecContext(ctx, `
		DELETE FROM virtual_items
		WHERE item_type NOT IN ('Series', 'Season')
		  AND NOT EXISTS (SELECT 1 FROM item_sources s WHERE s.virtual_item_id = virtual_items.id)
	`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	totalDeleted += n

	// 2. Delete Seasons with no child episodes and no sources.
	res, err = tx.ExecContext(ctx, `
		DELETE FROM virtual_items
		WHERE item_type = 'Season'
		  AND NOT EXISTS (SELECT 1 FROM item_sources s WHERE s.virtual_item_id = virtual_items.id)
		  AND NOT EXISTS (SELECT 1 FROM virtual_items c WHERE c.season_id = virtual_items.id OR c.parent_id = virtual_items.id)
	`)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	totalDeleted += n

	// 3. Delete Series with no child seasons/episodes and no sources.
	res, err = tx.ExecContext(ctx, `
		DELETE FROM virtual_items
		WHERE item_type = 'Series'
		  AND NOT EXISTS (SELECT 1 FROM item_sources s WHERE s.virtual_item_id = virtual_items.id)
		  AND NOT EXISTS (SELECT 1 FROM virtual_items c WHERE c.series_id = virtual_items.id OR c.parent_id = virtual_items.id)
	`)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	totalDeleted += n

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return totalDeleted, nil
}

// --- ItemSources ---

const sourceSelectCols = `
	s.source_id, s.virtual_item_id, s.server_id, s.remote_item_id, s.remote_media_source_id,
	s.label, s.container, s.video_codec, s.audio_codec,
	s.width, s.height, s.bitrate, s.size_bytes, s.run_time_ticks,
	s.raw_media_streams_json, s.raw_media_source_json
`

func scanItemSource(s rowScanner) (ItemSource, error) {
	var src ItemSource
	err := s.Scan(
		&src.SourceID, &src.VirtualItemID, &src.ServerID, &src.RemoteItemID, &src.RemoteMediaSourceID,
		&src.Label, &src.Container, &src.VideoCodec, &src.AudioCodec,
		&src.Width, &src.Height, &src.Bitrate, &src.SizeBytes, &src.RunTimeTicks,
		&src.RawMediaStreamsJSON, &src.RawMediaSourceJSON,
	)
	return src, err
}

func (d *DB) UpsertSource(ctx context.Context, s ItemSource) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO item_sources(
			source_id, virtual_item_id, server_id, remote_item_id, remote_media_source_id,
			label, container, video_codec, audio_codec,
			width, height, bitrate, size_bytes, run_time_ticks,
			raw_media_streams_json, raw_media_source_json
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id) DO UPDATE SET
			virtual_item_id = excluded.virtual_item_id,
			server_id = excluded.server_id,
			remote_item_id = excluded.remote_item_id,
			remote_media_source_id = excluded.remote_media_source_id,
			label = excluded.label,
			container = excluded.container,
			video_codec = excluded.video_codec,
			audio_codec = excluded.audio_codec,
			width = excluded.width,
			height = excluded.height,
			bitrate = excluded.bitrate,
			size_bytes = excluded.size_bytes,
			run_time_ticks = excluded.run_time_ticks,
			raw_media_streams_json = excluded.raw_media_streams_json,
			raw_media_source_json = excluded.raw_media_source_json
	`,
		s.SourceID, s.VirtualItemID, s.ServerID, s.RemoteItemID, s.RemoteMediaSourceID,
		s.Label, s.Container, s.VideoCodec, s.AudioCodec,
		s.Width, s.Height, s.Bitrate, s.SizeBytes, s.RunTimeTicks,
		defaultJSON(s.RawMediaStreamsJSON, "[]"), defaultJSON(s.RawMediaSourceJSON, "{}"),
	)
	return err
}

func (d *DB) ListSourcesByItemIDs(ctx context.Context, itemIDs []string) (map[string][]ItemSource, error) {
	result := make(map[string][]ItemSource)
	if len(itemIDs) == 0 {
		return result, nil
	}

	args := make([]any, len(itemIDs))
	for i, id := range itemIDs {
		args[i] = id
	}

	rows, err := d.sqlDB.QueryContext(ctx, `
		SELECT `+sourceSelectCols+`
		FROM item_sources s
		LEFT JOIN upstream_servers us ON us.id = s.server_id
		WHERE s.virtual_item_id IN (`+placeholders(len(itemIDs))+`)
		ORDER BY COALESCE(us.priority, 100) ASC, s.bitrate DESC, s.source_id ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		src, err := scanItemSource(rows)
		if err != nil {
			return nil, err
		}
		result[src.VirtualItemID] = append(result[src.VirtualItemID], src)
	}
	return result, rows.Err()
}

func (d *DB) ListSourcesByVirtualItemID(ctx context.Context, virtualItemID string) ([]ItemSource, error) {
	rows, err := d.sqlDB.QueryContext(ctx, `
		SELECT `+sourceSelectCols+`
		FROM item_sources s
		LEFT JOIN upstream_servers us ON us.id = s.server_id
		WHERE s.virtual_item_id = ?
		ORDER BY COALESCE(us.priority, 100) ASC, s.bitrate DESC, s.source_id ASC
	`, virtualItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ItemSource
	for rows.Next() {
		src, err := scanItemSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func (d *DB) ListAllSourcesForItem(ctx context.Context, virtualItemID string) ([]ItemSource, error) {
	return d.ListSourcesByVirtualItemID(ctx, virtualItemID)
}

func (d *DB) GetSourceByID(ctx context.Context, sourceID string) (*ItemSource, error) {
	src, err := scanItemSource(d.sqlDB.QueryRowContext(ctx, `
		SELECT `+sourceSelectCols+` FROM item_sources s WHERE s.source_id = ?
	`, sourceID))
	if err != nil {
		return nil, err
	}
	return &src, nil
}

func (d *DB) DeleteServerSourcesNotIn(ctx context.Context, serverID string, keepSourceIDs []string) (int64, error) {
	if len(keepSourceIDs) == 0 {
		res, err := d.sqlDB.ExecContext(ctx, `DELETE FROM item_sources WHERE server_id = ?`, serverID)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, nil
	}

	args := make([]any, 0, 1+len(keepSourceIDs))
	args = append(args, serverID)
	for _, id := range keepSourceIDs {
		args = append(args, id)
	}

	res, err := d.sqlDB.ExecContext(ctx, `
		DELETE FROM item_sources
		WHERE server_id = ? AND source_id NOT IN (`+placeholders(len(keepSourceIDs))+`)
	`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- UserItemData ---

func scanUserItemData(s rowScanner) (UserItemData, error) {
	var d UserItemData
	var played, isFav int
	err := s.Scan(&d.UserID, &d.VirtualItemID, &played, &d.PlaybackPositionTicks, &d.PlayCount, &isFav, &d.LastPlayedAt)
	d.Played = played != 0
	d.IsFavorite = isFav != 0
	return d, err
}

func (d *DB) GetUserItemData(ctx context.Context, userID, virtualItemID string) (*UserItemData, error) {
	uid, err := scanUserItemData(d.sqlDB.QueryRowContext(ctx, `
		SELECT user_id, virtual_item_id, played, playback_position_ticks, play_count, is_favorite, last_played_at
		FROM user_item_data
		WHERE user_id = ? AND virtual_item_id = ?
	`, userID, virtualItemID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &UserItemData{UserID: userID, VirtualItemID: virtualItemID}, nil
		}
		return nil, err
	}
	return &uid, nil
}

func (d *DB) BatchUserItemData(ctx context.Context, userID string, itemIDs []string) (map[string]UserItemData, error) {
	result := make(map[string]UserItemData)
	if len(itemIDs) == 0 {
		return result, nil
	}

	args := make([]any, 0, 1+len(itemIDs))
	args = append(args, userID)
	for _, id := range itemIDs {
		args = append(args, id)
	}

	rows, err := d.sqlDB.QueryContext(ctx, `
		SELECT user_id, virtual_item_id, played, playback_position_ticks, play_count, is_favorite, last_played_at
		FROM user_item_data
		WHERE user_id = ? AND virtual_item_id IN (`+placeholders(len(itemIDs))+`)
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		uid, err := scanUserItemData(rows)
		if err != nil {
			return nil, err
		}
		result[uid.VirtualItemID] = uid
	}
	return result, rows.Err()
}

func (d *DB) SetPlayed(ctx context.Context, userID, virtualItemID string, played bool, lastPlayedAt string) error {
	playCountInc := 0
	if played {
		playCountInc = 1
	}
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO user_item_data(user_id, virtual_item_id, played, playback_position_ticks, play_count, last_played_at)
		VALUES(?, ?, ?, 0, ?, ?)
		ON CONFLICT(user_id, virtual_item_id) DO UPDATE SET
			played = excluded.played,
			playback_position_ticks = 0,
			play_count = CASE WHEN excluded.played = 1 THEN user_item_data.play_count + 1 ELSE user_item_data.play_count END,
			last_played_at = CASE WHEN excluded.last_played_at != '' THEN excluded.last_played_at ELSE user_item_data.last_played_at END
	`, userID, virtualItemID, boolToInt(played), playCountInc, lastPlayedAt)
	return err
}

func (d *DB) SetFavorite(ctx context.Context, userID, virtualItemID string, isFavorite bool) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO user_item_data(user_id, virtual_item_id, is_favorite)
		VALUES(?, ?, ?)
		ON CONFLICT(user_id, virtual_item_id) DO UPDATE SET
			is_favorite = excluded.is_favorite
	`, userID, virtualItemID, boolToInt(isFavorite))
	return err
}

func (d *DB) UpsertProgress(ctx context.Context, userID, virtualItemID string, positionTicks int64, played bool, lastPlayedAt string) error {
	playCountInit := 0
	if played {
		playCountInit = 1
	}
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO user_item_data(user_id, virtual_item_id, played, playback_position_ticks, play_count, last_played_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, virtual_item_id) DO UPDATE SET
			played = excluded.played,
			playback_position_ticks = excluded.playback_position_ticks,
			play_count = CASE
				WHEN excluded.played = 1 AND user_item_data.played = 0 THEN user_item_data.play_count + 1
				ELSE user_item_data.play_count
			END,
			last_played_at = CASE WHEN excluded.last_played_at != '' THEN excluded.last_played_at ELSE user_item_data.last_played_at END
	`, userID, virtualItemID, boolToInt(played), positionTicks, playCountInit, lastPlayedAt)
	return err
}

func (d *DB) MergeInitialUserItemData(ctx context.Context, data UserItemData) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO user_item_data(user_id, virtual_item_id, played, playback_position_ticks, play_count, is_favorite, last_played_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, virtual_item_id) DO UPDATE SET
			played = MAX(user_item_data.played, excluded.played),
			playback_position_ticks = MAX(user_item_data.playback_position_ticks, excluded.playback_position_ticks),
			play_count = MAX(user_item_data.play_count, excluded.play_count),
			is_favorite = MAX(user_item_data.is_favorite, excluded.is_favorite),
			last_played_at = CASE
				WHEN excluded.last_played_at > user_item_data.last_played_at THEN excluded.last_played_at
				ELSE user_item_data.last_played_at
			END
	`, data.UserID, data.VirtualItemID, boolToInt(data.Played), data.PlaybackPositionTicks, data.PlayCount, boolToInt(data.IsFavorite), data.LastPlayedAt)
	return err
}

func (d *DB) CountUnplayedInSubtree(ctx context.Context, userID, parentID string) (int, error) {
	var count int
	err := d.sqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM virtual_items vi
		LEFT JOIN user_item_data uid ON uid.virtual_item_id = vi.id AND uid.user_id = ?
		WHERE vi.item_type IN ('Movie', 'Episode')
		  AND (vi.library_id = ? OR vi.parent_id = ? OR vi.series_id = ? OR vi.season_id = ?)
		  AND COALESCE(uid.played, 0) = 0
	`, userID, parentID, parentID, parentID, parentID).Scan(&count)
	return count, err
}

// --- ActivePlaySessions ---

const playSessionCols = `
	play_session_id, user_id, device_id, virtual_item_id, source_id,
	server_id, remote_item_id, remote_play_session_id, upstream_transcoding_url, started_at
`

func scanPlaySession(s rowScanner) (ActivePlaySession, error) {
	var ps ActivePlaySession
	err := s.Scan(
		&ps.PlaySessionID, &ps.UserID, &ps.DeviceID, &ps.VirtualItemID, &ps.SourceID,
		&ps.ServerID, &ps.RemoteItemID, &ps.RemotePlaySessionID, &ps.UpstreamTranscodingURL, &ps.StartedAt,
	)
	return ps, err
}

func (d *DB) CreatePlaySession(ctx context.Context, s ActivePlaySession) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO active_play_sessions(
			play_session_id, user_id, device_id, virtual_item_id, source_id,
			server_id, remote_item_id, remote_play_session_id, upstream_transcoding_url, started_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(play_session_id) DO UPDATE SET
			user_id = excluded.user_id,
			device_id = excluded.device_id,
			virtual_item_id = excluded.virtual_item_id,
			source_id = excluded.source_id,
			server_id = excluded.server_id,
			remote_item_id = excluded.remote_item_id,
			remote_play_session_id = excluded.remote_play_session_id,
			upstream_transcoding_url = excluded.upstream_transcoding_url,
			started_at = CURRENT_TIMESTAMP
	`, s.PlaySessionID, s.UserID, s.DeviceID, s.VirtualItemID, s.SourceID,
		s.ServerID, s.RemoteItemID, s.RemotePlaySessionID, s.UpstreamTranscodingURL)
	return err
}

func (d *DB) GetPlaySession(ctx context.Context, playSessionID string) (*ActivePlaySession, error) {
	ps, err := scanPlaySession(d.sqlDB.QueryRowContext(ctx, `
		SELECT `+playSessionCols+` FROM active_play_sessions WHERE play_session_id = ?
	`, playSessionID))
	if err != nil {
		return nil, err
	}
	return &ps, nil
}

func (d *DB) GetLatestPlaySessionByDevice(ctx context.Context, userID, deviceID, virtualItemID string) (*ActivePlaySession, error) {
	ps, err := scanPlaySession(d.sqlDB.QueryRowContext(ctx, `
		SELECT `+playSessionCols+`
		FROM active_play_sessions
		WHERE user_id = ? AND device_id = ? AND virtual_item_id = ?
		ORDER BY started_at DESC, rowid DESC
		LIMIT 1
	`, userID, deviceID, virtualItemID))
	if err != nil {
		return nil, err
	}
	return &ps, nil
}

func (d *DB) DeletePlaySession(ctx context.Context, playSessionID string) error {
	_, err := d.sqlDB.ExecContext(ctx, `DELETE FROM active_play_sessions WHERE play_session_id = ?`, playSessionID)
	return err
}
