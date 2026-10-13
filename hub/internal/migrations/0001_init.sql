-- Cercle hub schema v1. All times are unix epoch milliseconds (INTEGER).
-- Columns named user_id hold the Navidrome username (users.username).

CREATE TABLE users (
    username              TEXT PRIMARY KEY,
    display_name          TEXT    NOT NULL,
    display_name_custom   INTEGER NOT NULL DEFAULT 0,
    navidrome_id          TEXT    NOT NULL DEFAULT '',
    is_admin              INTEGER NOT NULL DEFAULT 0,
    created_at            INTEGER NOT NULL,
    last_seen_at          INTEGER,
    paused_until          INTEGER,
    history_delay_minutes INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE hidden_items (
    username   TEXT    NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    kind       TEXT    NOT NULL CHECK (kind IN ('artist', 'album', 'track')),
    item_id    TEXT    NOT NULL,
    label      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (username, kind, item_id)
);

CREATE TABLE sessions (
    token_hash   TEXT PRIMARY KEY,
    username     TEXT    NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    refreshed_at INTEGER NOT NULL
);
CREATE INDEX sessions_expires ON sessions(expires_at);

CREATE TABLE circles (
    id          TEXT PRIMARY KEY,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL DEFAULT 'custom' CHECK (kind IN ('everyone', 'custom')),
    created_by  TEXT,
    created_at  INTEGER NOT NULL
);

CREATE TABLE memberships (
    circle_id         TEXT    NOT NULL REFERENCES circles(id) ON DELETE CASCADE,
    username          TEXT    NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    role              TEXT    NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member')),
    joined_at         INTEGER NOT NULL,
    share_live        INTEGER NOT NULL DEFAULT 1,
    share_history     INTEGER NOT NULL DEFAULT 1,
    share_stats       INTEGER NOT NULL DEFAULT 1,
    share_discoveries INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (circle_id, username)
);
CREATE INDEX memberships_user ON memberships(username, circle_id);

CREATE TABLE invites (
    code       TEXT PRIMARY KEY,
    circle_id  TEXT    NOT NULL REFERENCES circles(id) ON DELETE CASCADE,
    created_by TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    max_uses   INTEGER NOT NULL DEFAULT 0,
    uses       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX invites_circle ON invites(circle_id);

CREATE TABLE plays (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id       TEXT    NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    track_id      TEXT    NOT NULL,
    title         TEXT    NOT NULL DEFAULT '',
    artist        TEXT    NOT NULL DEFAULT '',
    artist_id     TEXT    NOT NULL DEFAULT '',
    album         TEXT    NOT NULL DEFAULT '',
    album_id      TEXT    NOT NULL DEFAULT '',
    album_artist  TEXT    NOT NULL DEFAULT '',
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    track_number  INTEGER NOT NULL DEFAULT 0,
    year          INTEGER NOT NULL DEFAULT 0,
    genre         TEXT    NOT NULL DEFAULT '',
    mbz_track_id  TEXT    NOT NULL DEFAULT '',
    mbz_album_id  TEXT    NOT NULL DEFAULT '',
    mbz_artist_id TEXT    NOT NULL DEFAULT '',
    player_id     TEXT    NOT NULL DEFAULT '',
    player_name   TEXT    NOT NULL DEFAULT '',
    played_at     INTEGER NOT NULL,
    private       INTEGER NOT NULL DEFAULT 0,
    feed_item_id  TEXT,
    created_at    INTEGER NOT NULL
);
CREATE INDEX plays_user_played ON plays(user_id, played_at);
CREATE INDEX plays_user_artist ON plays(user_id, artist_id);
CREATE INDEX plays_user_album  ON plays(user_id, album_id);
CREATE INDEX plays_user_track  ON plays(user_id, track_id, played_at);
CREATE INDEX plays_feed_item   ON plays(feed_item_id);

-- Feed items are maintained at ingest time. id = first play id for
-- 'listened' groups, '<play id>-artist' / '<play id>-album' for discoveries.
CREATE TABLE feed_items (
    id             TEXT PRIMARY KEY,
    kind           TEXT    NOT NULL CHECK (kind IN ('listened', 'discovery_artist', 'discovery_album')),
    user_id        TEXT    NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    first_play_id  INTEGER NOT NULL,
    last_play_id   INTEGER NOT NULL,
    album_key      TEXT    NOT NULL DEFAULT '',
    play_count     INTEGER NOT NULL DEFAULT 1,
    started_at     INTEGER NOT NULL,
    ended_at       INTEGER NOT NULL,
    last_played_at INTEGER NOT NULL,
    private        INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL
);
CREATE INDEX feed_items_user_ended ON feed_items(user_id, ended_at);
CREATE INDEX feed_items_ended ON feed_items(ended_at, id);

CREATE TABLE reactions (
    item_id    TEXT    NOT NULL REFERENCES feed_items(id) ON DELETE CASCADE,
    user_id    TEXT    NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    emoji      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (item_id, user_id, emoji)
);

CREATE TABLE comments (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id    TEXT    NOT NULL REFERENCES feed_items(id) ON DELETE CASCADE,
    user_id    TEXT    NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    body       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    edited_at  INTEGER
);
CREATE INDEX comments_item ON comments(item_id, id);

CREATE TABLE rooms (
    id         TEXT PRIMARY KEY,
    circle_id  TEXT    NOT NULL REFERENCES circles(id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    host       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    state      TEXT    NOT NULL,
    seq        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX rooms_circle ON rooms(circle_id);

CREATE TABLE room_listeners (
    room_id   TEXT    NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    user_id   TEXT    NOT NULL,
    joined_at INTEGER NOT NULL,
    PRIMARY KEY (room_id, user_id)
);

CREATE TABLE room_requests (
    id         TEXT PRIMARY KEY,
    room_id    TEXT    NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    user_id    TEXT    NOT NULL,
    track      TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);

INSERT INTO circles (id, name, description, kind, created_by, created_at)
VALUES ('everyone', 'Everyone', 'Everyone on this server', 'everyone', NULL,
        CAST(strftime('%s', 'now') AS INTEGER) * 1000);
