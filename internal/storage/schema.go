package storage

// schemaV1 is the complete initial schema, ported table-by-table from
// anicli-py: ORM models (anicli/db/models.py: anime_progress,
// predicted_skip_time) and the alembic tables (anime_episode_progress +
// api_auth_session from 20260404_000001, shikimori_anime_details_cache from
// 20260415_000002, anime_source + auto_download_rule from 20260628_000003)
// plus provider_search_stat and the go-only columns dirty and
// progress_seconds/total_seconds (spec §6 storage note; the seconds pair is
// a last-known-playback snapshot for quick history rendering).
//
// Index and constraint names follow the Python originals (uix_*/ix_*).
// api_auth_session has no repository yet; it ships with the API face (G5).
const schemaV1 = `
CREATE TABLE anime_progress (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	title              TEXT NOT NULL,
	poster             TEXT,
	source_id          TEXT NOT NULL,
	source_url         TEXT NOT NULL,
	current_episode    TEXT NOT NULL DEFAULT '1',
	video_dub          TEXT,
	audio_dub          TEXT,
	shikimori_title    TEXT,
	bound_title        TEXT,
	bound_similarity   REAL,
	shikimori_id       INTEGER,
	shikimori_rate_id  INTEGER,
	shikimori_status   TEXT NOT NULL DEFAULT 'watching',
	score              INTEGER NOT NULL DEFAULT 0,
	total_episodes     INTEGER NOT NULL DEFAULT 0,
	rewatches          INTEGER NOT NULL DEFAULT 0,
	needs_correction   INTEGER NOT NULL DEFAULT 0,
	progress_seconds   INTEGER NOT NULL DEFAULT 0,
	total_seconds      INTEGER NOT NULL DEFAULT 0,
	dirty              INTEGER NOT NULL DEFAULT 0,
	updated_at         TEXT NOT NULL
);
CREATE UNIQUE INDEX uix_source_anime ON anime_progress (source_id, source_url);
CREATE INDEX ix_anime_progress_title ON anime_progress (title);
CREATE INDEX ix_anime_progress_shikimori_id ON anime_progress (shikimori_id);
CREATE INDEX ix_anime_progress_shikimori_status ON anime_progress (shikimori_status);
CREATE INDEX ix_anime_progress_updated_at ON anime_progress (updated_at);

CREATE TABLE anime_episode_progress (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	anime_id     INTEGER NOT NULL REFERENCES anime_progress (id) ON DELETE CASCADE,
	episode      TEXT NOT NULL,
	position_sec INTEGER NOT NULL DEFAULT 0,
	duration_sec INTEGER NOT NULL DEFAULT 0,
	video_key    TEXT,
	audio_key    TEXT,
	quality      INTEGER,
	updated_at   TEXT NOT NULL
);
CREATE UNIQUE INDEX uix_anime_episode_progress_anime_id ON anime_episode_progress (anime_id, episode);
CREATE INDEX ix_anime_episode_progress_anime_id ON anime_episode_progress (anime_id);
CREATE INDEX ix_anime_episode_progress_episode ON anime_episode_progress (episode);

CREATE TABLE predicted_skip_time (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	shikimori_id    INTEGER NOT NULL,
	episode_num     REAL NOT NULL,
	skip_type       TEXT NOT NULL,
	start_time      REAL NOT NULL,
	end_time        REAL NOT NULL,
	episode_length  REAL NOT NULL DEFAULT 0.0,
	created_at      TEXT NOT NULL
);
CREATE UNIQUE INDEX uix_skip ON predicted_skip_time (shikimori_id, episode_num, skip_type);
CREATE INDEX ix_predicted_skip_time_shikimori_id ON predicted_skip_time (shikimori_id);

CREATE TABLE shikimori_anime_details_cache (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	anime_id            INTEGER NOT NULL,
	payload_json        TEXT NOT NULL,
	immutable_cached_at TEXT NOT NULL,
	mutable_updated_at  TEXT NOT NULL,
	updated_at          TEXT NOT NULL
);
CREATE UNIQUE INDEX uix_shikimori_anime_details_cache_anime_id ON shikimori_anime_details_cache (anime_id);
CREATE INDEX ix_shikimori_anime_details_cache_anime_id ON shikimori_anime_details_cache (anime_id);

CREATE TABLE anime_source (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	anime_progress_id INTEGER NOT NULL REFERENCES anime_progress (id) ON DELETE CASCADE,
	source_id         TEXT NOT NULL,
	source_url        TEXT NOT NULL,
	video_dub         TEXT,
	audio_dub         TEXT,
	quality           INTEGER,
	last_resolved_at  TEXT,
	created_at        TEXT NOT NULL
);
CREATE UNIQUE INDEX uix_anime_source ON anime_source (anime_progress_id, source_id);
CREATE INDEX ix_anime_source_anime_progress_id ON anime_source (anime_progress_id);

CREATE TABLE auto_download_rule (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	anime_id            INTEGER NOT NULL REFERENCES anime_progress (id) ON DELETE CASCADE,
	enabled             INTEGER NOT NULL DEFAULT 1,
	preferred_source_id TEXT,
	preferred_video_dub TEXT,
	preferred_quality   INTEGER NOT NULL DEFAULT 1080,
	max_new_episodes    INTEGER NOT NULL DEFAULT 1,
	title               TEXT,
	created_at          TEXT NOT NULL,
	updated_at          TEXT NOT NULL
);
CREATE UNIQUE INDEX uix_auto_download_rule_anime_id ON auto_download_rule (anime_id);
CREATE INDEX ix_auto_download_rule_anime_id ON auto_download_rule (anime_id);

CREATE TABLE api_auth_session (
	id                   INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id           TEXT NOT NULL,
	user_login           TEXT NOT NULL,
	refresh_token_hash   TEXT NOT NULL,
	shiki_auth_mode      TEXT,
	shiki_username       TEXT,
	shiki_cookie_session TEXT,
	shiki_access_token   TEXT,
	expires_at           TEXT NOT NULL,
	revoked_at           TEXT,
	created_at           TEXT NOT NULL,
	updated_at           TEXT NOT NULL
);
CREATE UNIQUE INDEX uix_api_auth_session_id ON api_auth_session (session_id);
CREATE UNIQUE INDEX uix_api_auth_refresh_hash ON api_auth_session (refresh_token_hash);
CREATE INDEX ix_api_auth_session_session_id ON api_auth_session (session_id);
CREATE INDEX ix_api_auth_session_user_login ON api_auth_session (user_login);
CREATE INDEX ix_api_auth_session_refresh_token_hash ON api_auth_session (refresh_token_hash);
CREATE INDEX ix_api_auth_session_expires_at ON api_auth_session (expires_at);

CREATE TABLE provider_search_stat (
	provider_id    TEXT PRIMARY KEY,
	successes      INTEGER NOT NULL DEFAULT 0,
	failures       INTEGER NOT NULL DEFAULT 0,
	avg_latency_ms REAL NOT NULL DEFAULT 0,
	last_used_at   TEXT NOT NULL
);
`
