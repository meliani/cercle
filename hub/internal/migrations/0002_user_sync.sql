-- Navidrome user sync: users deleted from Navidrome are disabled, not deleted.
-- (users.display_name_custom already exists since 0001.)
ALTER TABLE users ADD COLUMN disabled_at INTEGER;

-- Memberships of active users only. Every *read* that decides visibility,
-- member lists or member counts goes through this view, so a disabled user
-- vanishes everywhere while their data stays intact for re-enabling.
CREATE VIEW active_memberships AS
    SELECT m.* FROM memberships m
    JOIN users u ON u.username = m.username
    WHERE u.disabled_at IS NULL;
