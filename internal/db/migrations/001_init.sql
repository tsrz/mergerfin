CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_users (
    id TEXT PRIMARY KEY,
    username TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    is_admin INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS proxy_sessions (
    token TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES proxy_users(id) ON DELETE CASCADE,
    device_id TEXT NOT NULL DEFAULT '',
    client_name TEXT NOT NULL DEFAULT '',
    device_name TEXT NOT NULL DEFAULT '',
    app_version TEXT NOT NULL DEFAULT '',
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_proxy_sessions_user_id ON proxy_sessions(user_id);

CREATE TABLE IF NOT EXISTS display_preferences (
    user_id TEXT NOT NULL,
    pref_id TEXT NOT NULL,
    client TEXT NOT NULL,
    json_payload TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(user_id, pref_id, client)
);

CREATE TABLE IF NOT EXISTS upstream_servers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    base_url TEXT NOT NULL,
    username TEXT NOT NULL,
    remote_user_id TEXT NOT NULL DEFAULT '',
    access_token TEXT NOT NULL DEFAULT '',
    priority INTEGER NOT NULL DEFAULT 100,
    stream_mode TEXT NOT NULL DEFAULT 'proxy',
    enabled INTEGER NOT NULL DEFAULT 1,
    server_version TEXT NOT NULL DEFAULT '',
    is_online INTEGER NOT NULL DEFAULT 1,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    last_sync_at DATETIME,
    last_error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS virtual_libraries (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    collection_type TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS virtual_items (
    id TEXT PRIMARY KEY,
    library_id TEXT NOT NULL,
    parent_id TEXT NOT NULL DEFAULT '',
    series_id TEXT NOT NULL DEFAULT '',
    season_id TEXT NOT NULL DEFAULT '',
    item_type TEXT NOT NULL,
    canonical_key TEXT UNIQUE NOT NULL,
    name TEXT NOT NULL,
    sort_name TEXT NOT NULL DEFAULT '',
    original_title TEXT NOT NULL DEFAULT '',
    overview TEXT NOT NULL DEFAULT '',
    tagline TEXT NOT NULL DEFAULT '',
    production_year INTEGER NOT NULL DEFAULT 0,
    premiere_date TEXT NOT NULL DEFAULT '',
    community_rating REAL NOT NULL DEFAULT 0,
    official_rating TEXT NOT NULL DEFAULT '',
    index_number INTEGER NOT NULL DEFAULT 0,
    parent_index_number INTEGER NOT NULL DEFAULT 0,
    run_time_ticks INTEGER NOT NULL DEFAULT 0,
    series_name TEXT NOT NULL DEFAULT '',
    season_name TEXT NOT NULL DEFAULT '',
    genres_json TEXT NOT NULL DEFAULT '[]',
    studios_json TEXT NOT NULL DEFAULT '[]',
    people_json TEXT NOT NULL DEFAULT '[]',
    provider_ids_json TEXT NOT NULL DEFAULT '{}',
    img_primary_server_id TEXT NOT NULL DEFAULT '',
    img_primary_remote_id TEXT NOT NULL DEFAULT '',
    img_backdrop_server_id TEXT NOT NULL DEFAULT '',
    img_backdrop_remote_id TEXT NOT NULL DEFAULT '',
    date_created TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_virtual_items_library_id ON virtual_items(library_id);
CREATE INDEX IF NOT EXISTS idx_virtual_items_parent_id ON virtual_items(parent_id);
CREATE INDEX IF NOT EXISTS idx_virtual_items_series_id ON virtual_items(series_id);
CREATE INDEX IF NOT EXISTS idx_virtual_items_season_id ON virtual_items(season_id);
CREATE INDEX IF NOT EXISTS idx_virtual_items_item_type ON virtual_items(item_type);
CREATE INDEX IF NOT EXISTS idx_virtual_items_sort_name ON virtual_items(sort_name);
CREATE INDEX IF NOT EXISTS idx_virtual_items_date_created ON virtual_items(date_created);

CREATE VIRTUAL TABLE IF NOT EXISTS virtual_items_fts USING fts5(
    item_id UNINDEXED,
    name,
    original_title,
    overview
);

CREATE TRIGGER IF NOT EXISTS virtual_items_ai AFTER INSERT ON virtual_items BEGIN
    INSERT INTO virtual_items_fts(item_id, name, original_title, overview)
    VALUES (NEW.id, NEW.name, NEW.original_title, NEW.overview);
END;

CREATE TRIGGER IF NOT EXISTS virtual_items_au AFTER UPDATE ON virtual_items BEGIN
    DELETE FROM virtual_items_fts WHERE item_id = OLD.id;
    INSERT INTO virtual_items_fts(item_id, name, original_title, overview)
    VALUES (NEW.id, NEW.name, NEW.original_title, NEW.overview);
END;

CREATE TRIGGER IF NOT EXISTS virtual_items_ad AFTER DELETE ON virtual_items BEGIN
    DELETE FROM virtual_items_fts WHERE item_id = OLD.id;
END;

CREATE TABLE IF NOT EXISTS item_sources (
    source_id TEXT PRIMARY KEY,
    virtual_item_id TEXT NOT NULL REFERENCES virtual_items(id) ON DELETE CASCADE,
    server_id TEXT NOT NULL REFERENCES upstream_servers(id) ON DELETE CASCADE,
    remote_item_id TEXT NOT NULL,
    remote_media_source_id TEXT NOT NULL,
    label TEXT NOT NULL,
    container TEXT NOT NULL DEFAULT '',
    video_codec TEXT NOT NULL DEFAULT '',
    audio_codec TEXT NOT NULL DEFAULT '',
    width INTEGER NOT NULL DEFAULT 0,
    height INTEGER NOT NULL DEFAULT 0,
    bitrate INTEGER NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    run_time_ticks INTEGER NOT NULL DEFAULT 0,
    raw_media_streams_json TEXT NOT NULL DEFAULT '[]',
    raw_media_source_json TEXT NOT NULL DEFAULT '{}',
    UNIQUE(server_id, remote_media_source_id)
);

CREATE INDEX IF NOT EXISTS idx_item_sources_virtual_item_id ON item_sources(virtual_item_id);
CREATE INDEX IF NOT EXISTS idx_item_sources_server_id ON item_sources(server_id);

CREATE TABLE IF NOT EXISTS user_item_data (
    user_id TEXT NOT NULL,
    virtual_item_id TEXT NOT NULL REFERENCES virtual_items(id) ON DELETE CASCADE,
    played INTEGER NOT NULL DEFAULT 0,
    playback_position_ticks INTEGER NOT NULL DEFAULT 0,
    play_count INTEGER NOT NULL DEFAULT 0,
    is_favorite INTEGER NOT NULL DEFAULT 0,
    last_played_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, virtual_item_id)
);

CREATE TABLE IF NOT EXISTS active_play_sessions (
    play_session_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL DEFAULT '',
    device_id TEXT NOT NULL DEFAULT '',
    virtual_item_id TEXT NOT NULL DEFAULT '',
    source_id TEXT NOT NULL DEFAULT '',
    server_id TEXT NOT NULL DEFAULT '',
    remote_item_id TEXT NOT NULL DEFAULT '',
    remote_play_session_id TEXT NOT NULL DEFAULT '',
    upstream_transcoding_url TEXT NOT NULL DEFAULT '',
    started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
