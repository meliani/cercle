// Package users manages hub user records and per-user privacy settings.
package users

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/model"
)

// Columns selected by Scan, in order.
const Cols = "username, display_name, is_admin, created_at, last_seen_at"

// ErrExcluded is returned by Ensure for excluded service accounts.
var ErrExcluded = errors.New("excluded user")

// Service reads and writes users.
type Service struct {
	DB  *db.DB
	Now func() time.Time
	// Excluded reports whether a username is an excluded service account
	// (CERCLE_EXCLUDED_USERS). Nil means nobody is excluded.
	Excluded func(username string) bool

	mu   sync.Mutex
	seen map[string]time.Time
}

func New(d *db.DB, now func() time.Time) *Service {
	return &Service{DB: d, Now: now, seen: map[string]time.Time{}}
}

// EnsureOpts carries optional metadata learned about a user.
type EnsureOpts struct {
	Name        string // display name from Navidrome (only applied if not customised)
	NavidromeID string
	IsAdmin     *bool
}

// IsExcluded reports whether username is an excluded service account.
func (s *Service) IsExcluded(username string) bool {
	return s.Excluded != nil && s.Excluded(username)
}

// Ensure creates the user if missing — joining them to the `everyone` circle,
// which is how "visible from the start" works — and refreshes metadata. A
// disabled user (deleted from Navidrome at the last sync) is re-enabled:
// whoever calls Ensure has just proven the account exists. Excluded service
// accounts are refused with ErrExcluded and never created.
// It must be called with the caller's write transaction.
func (s *Service) Ensure(ctx context.Context, q db.Querier, username string, o EnsureOpts) (created bool, err error) {
	if s.IsExcluded(username) {
		return false, ErrExcluded
	}
	now := s.Now().UnixMilli()
	display := o.Name
	if display == "" {
		display = username
	}
	admin := 0
	if o.IsAdmin != nil {
		admin = db.Bool(*o.IsAdmin)
	}
	res, err := q.ExecContext(ctx, `INSERT INTO users (username, display_name, navidrome_id, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(username) DO NOTHING`, username, display, o.NavidromeID, admin, now)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO memberships (circle_id, username, role, joined_at)
			VALUES ('everyone', ?, 'member', ?)`, username, now); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := q.ExecContext(ctx, `UPDATE users SET disabled_at = NULL WHERE username = ? AND disabled_at IS NOT NULL`, username); err != nil {
		return false, err
	}
	if o.Name != "" {
		if _, err := q.ExecContext(ctx, `UPDATE users SET display_name = ?
			WHERE username = ? AND display_name_custom = 0 AND display_name <> ?`, o.Name, username, o.Name); err != nil {
			return false, err
		}
	}
	if o.NavidromeID != "" {
		if _, err := q.ExecContext(ctx, `UPDATE users SET navidrome_id = ? WHERE username = ? AND navidrome_id <> ?`,
			o.NavidromeID, username, o.NavidromeID); err != nil {
			return false, err
		}
	}
	if o.IsAdmin != nil {
		if _, err := q.ExecContext(ctx, `UPDATE users SET is_admin = ? WHERE username = ? AND is_admin <> ?`,
			admin, username, admin); err != nil {
			return false, err
		}
	}
	return false, nil
}

// EnsureTx is Ensure in its own transaction.
func (s *Service) EnsureTx(ctx context.Context, username string, o EnsureOpts) error {
	return s.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := s.Ensure(ctx, tx, username, o)
		return err
	})
}

// Scanner is satisfied by *sql.Row and *sql.Rows.
type Scanner interface{ Scan(dest ...any) error }

// Scan reads the Cols columns.
func Scan(sc Scanner) (model.User, error) {
	var u model.User
	var created int64
	var seen sql.NullInt64
	var admin int
	if err := sc.Scan(&u.Username, &u.DisplayName, &admin, &created, &seen); err != nil {
		return u, err
	}
	u.IsAdmin = admin == 1
	u.CreatedAt = model.TS(created)
	if seen.Valid {
		u.LastSeenAt = model.TSPtr(&seen.Int64)
	}
	return u, nil
}

// Get loads one user.
func (s *Service) Get(ctx context.Context, username string) (model.User, error) {
	u, err := Scan(s.DB.R.QueryRowContext(ctx, `SELECT `+Cols+` FROM users WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return u, model.ErrNotFound
	}
	return u, err
}

// GetMany loads several users keyed by username (missing ones are absent).
func (s *Service) GetMany(ctx context.Context, names []string) (map[string]model.User, error) {
	out := make(map[string]model.User, len(names))
	uniq := make([]any, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			uniq = append(uniq, n)
		}
	}
	for len(uniq) > 0 {
		chunk := uniq
		if len(chunk) > 500 {
			chunk = chunk[:500]
		}
		uniq = uniq[len(chunk):]
		rows, err := s.DB.R.QueryContext(ctx, `SELECT `+Cols+` FROM users WHERE username IN (`+db.Placeholders(len(chunk))+`)`, chunk...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			u, err := Scan(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[u.Username] = u
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Settings loads a user's privacy settings.
func (s *Service) Settings(ctx context.Context, username string) (model.Settings, error) {
	var st model.Settings
	var paused sql.NullInt64
	err := s.DB.R.QueryRowContext(ctx, `SELECT paused_until, history_delay_minutes FROM users WHERE username = ?`, username).
		Scan(&paused, &st.HistoryDelayMinutes)
	if errors.Is(err, sql.ErrNoRows) {
		return st, model.ErrNotFound
	}
	if err != nil {
		return st, err
	}
	if paused.Valid && paused.Int64 > s.Now().UnixMilli() {
		st.PausedUntil = model.TSPtr(&paused.Int64)
	}
	st.Hidden = []model.Hidden{}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT kind, item_id, label FROM hidden_items WHERE username = ? ORDER BY created_at, kind, item_id`, username)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var h model.Hidden
		if err := rows.Scan(&h.Kind, &h.ID, &h.Label); err != nil {
			return st, err
		}
		st.Hidden = append(st.Hidden, h)
	}
	return st, rows.Err()
}

// Me loads the user together with their settings.
func (s *Service) Me(ctx context.Context, username string) (model.Me, error) {
	u, err := s.Get(ctx, username)
	if err != nil {
		return model.Me{}, err
	}
	st, err := s.Settings(ctx, username)
	return model.Me{User: u, Settings: st}, err
}

// Touch records activity, writing at most every 5 minutes per user.
func (s *Service) Touch(ctx context.Context, username string) {
	now := s.Now()
	s.mu.Lock()
	last, ok := s.seen[username]
	if ok && now.Sub(last) < 5*time.Minute {
		s.mu.Unlock()
		return
	}
	s.seen[username] = now
	s.mu.Unlock()
	_, _ = s.DB.W.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE username = ?`, now.UnixMilli(), username)
}
