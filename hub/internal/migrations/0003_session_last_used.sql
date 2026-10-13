-- When a session was last used (throttled to one write per 5 minutes). The
-- user sync never disables someone with a session used in the last 24 h.
ALTER TABLE sessions ADD COLUMN last_used_at INTEGER;
UPDATE sessions SET last_used_at = refreshed_at;
CREATE INDEX sessions_user ON sessions(username, last_used_at);
