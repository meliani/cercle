// Package usersync mirrors Navidrome's user list into the hub so that new
// users are in the `everyone` circle before they ever log in or play, and
// users deleted from Navidrome disappear (disabled, data kept).
//
// Disabling is the dangerous half, so it is fenced by guards; when any guard
// trips, creation/re-enabling still happens but nobody is disabled:
//
//	(a) the listing must contain every user active in the last 24 h
//	    (seen via API/ingest, or logged in);
//	(b) at most max(1, 20 % of active users) per run, unless
//	    CERCLE_USER_SYNC_ALLOW_MASS_DISABLE;
//	(c) a user with a valid session used in the last 24 h is never disabled;
//	(d) a run never disables every active user (not overridable).
package usersync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/meliani/cercle/hub/internal/auth"
	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/live"
	"github.com/meliani/cercle/hub/internal/users"
	"github.com/meliani/cercle/hub/internal/visibility"
)

// ActiveWindow is how recent activity must be to protect a user.
const ActiveWindow = 24 * time.Hour

// Syncer runs the sync.
type Syncer struct {
	Nav              auth.UserLister
	DB               *db.DB
	Users            *users.Service
	Vis              *visibility.V
	Live             *live.Service
	Now              func() time.Time
	Log              *slog.Logger
	AllowMassDisable bool
}

// Result summarises one run. DisableSkipped is non-empty when a guard
// stopped the disable step.
type Result struct {
	Seen, Created, Reenabled, Disabled, Excluded, Protected int
	DisableSkipped                                          string
}

type hubUser struct {
	name     string
	disabled bool
	recent   bool // active in the last 24 h (guard a)
	session  bool // valid session used in the last 24 h (guard c)
}

func (s *Syncer) loadUsers(ctx context.Context, now int64) ([]hubUser, error) {
	since := now - ActiveWindow.Milliseconds()
	rows, err := s.DB.R.QueryContext(ctx, `SELECT u.username, u.disabled_at IS NOT NULL,
		(COALESCE(u.last_seen_at, 0) >= ? OR EXISTS (SELECT 1 FROM sessions s WHERE s.username = u.username AND s.created_at >= ?)),
		EXISTS (SELECT 1 FROM sessions s WHERE s.username = u.username AND s.expires_at > ? AND COALESCE(s.last_used_at, s.refreshed_at) >= ?)
		FROM users u`, since, since, now, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hubUser
	for rows.Next() {
		var h hubUser
		if err := rows.Scan(&h.name, &h.disabled, &h.recent, &h.session); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// plan decides who to disable, applying the guards.
func (s *Syncer) plan(existing []hubUser, present map[string]bool, r *Result) []string {
	active, missingRecent := 0, 0
	var candidates []string
	for _, h := range existing {
		if h.disabled || s.Users.IsExcluded(h.name) {
			continue
		}
		active++
		if present[strings.ToLower(h.name)] {
			continue
		}
		if h.recent {
			missingRecent++
		}
		if h.session {
			r.Protected++ // (c)
			continue
		}
		candidates = append(candidates, h.name)
	}
	switch limit := max(1, active*20/100); {
	case missingRecent > 0: // (a)
		r.DisableSkipped = fmt.Sprintf("listing of %d users misses %d user(s) active in the last 24h", r.Seen, missingRecent)
	case len(candidates) > 0 && len(candidates) >= active: // (d)
		r.DisableSkipped = fmt.Sprintf("would disable all %d active users", active)
	case len(candidates) > limit && !s.AllowMassDisable: // (b)
		r.DisableSkipped = fmt.Sprintf("would disable %d of %d active users (limit %d; set CERCLE_USER_SYNC_ALLOW_MASS_DISABLE=true to allow)",
			len(candidates), active, limit)
	default:
		return candidates
	}
	return nil
}

// Sync runs once. On any Navidrome error nothing is changed.
func (s *Syncer) Sync(ctx context.Context) (Result, error) {
	var r Result
	list, err := s.Nav.ListUsers(ctx)
	if err != nil {
		return r, err
	}
	if len(list) == 0 {
		return r, errors.New("navidrome returned no users; skipping")
	}
	r.Seen = len(list)
	present := make(map[string]bool, len(list))
	for _, u := range list {
		present[strings.ToLower(u.Username)] = true
	}
	now := s.Now().UnixMilli()
	existing, err := s.loadUsers(ctx, now)
	if err != nil {
		return r, err
	}
	wasDisabled := map[string]bool{}
	for _, h := range existing {
		if h.disabled {
			wasDisabled[strings.ToLower(h.name)] = true
		}
	}
	toDisable := s.plan(existing, present, &r)
	if r.DisableSkipped != "" {
		s.Log.Warn("navidrome user sync: disable step skipped", "reason", r.DisableSkipped,
			"listed", r.Seen, "hub_users", len(existing))
	}
	if r.Protected > 0 {
		s.Log.Warn("navidrome user sync: users missing from Navidrome kept (recent session)", "count", r.Protected)
	}

	// Who might see a live state that is about to vanish (before disabling,
	// since a disabled user has no visible circle mates).
	liveMates := map[string][]string{}
	for _, u := range toDisable {
		if _, ok := s.Live.Get(u); ok {
			if m, err := s.Vis.Mates(ctx, u); err == nil {
				for v := range m {
					liveMates[u] = append(liveMates[u], v)
				}
			}
		}
	}

	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		for _, nu := range list {
			if s.Users.IsExcluded(nu.Username) {
				r.Excluded++
				continue
			}
			admin := nu.IsAdmin
			created, err := s.Users.Ensure(ctx, tx, nu.Username, users.EnsureOpts{Name: nu.Name, NavidromeID: nu.ID, IsAdmin: &admin})
			if err != nil {
				return err
			}
			if created {
				r.Created++
			} else if wasDisabled[strings.ToLower(nu.Username)] {
				r.Reenabled++
			}
		}
		for _, u := range toDisable {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = ? WHERE username = ? AND disabled_at IS NULL`, now, u); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE username = ?`, u); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	r.Disabled = len(toDisable)
	for _, u := range toDisable {
		s.Live.Drop(ctx, u, liveMates[u]...)
	}
	return r, nil
}

// Run syncs once immediately (closing first when done), then every interval
// until ctx ends.
func (s *Syncer) Run(ctx context.Context, every time.Duration, first chan<- struct{}) {
	s.runOnce(ctx)
	if first != nil {
		close(first)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Syncer) runOnce(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := s.Sync(cctx)
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Warn("navidrome user sync failed; nothing changed", "err", err)
		}
		return
	}
	s.Log.Info("navidrome user sync", "seen", r.Seen, "created", r.Created, "reenabled", r.Reenabled,
		"disabled", r.Disabled, "excluded", r.Excluded, "protected", r.Protected, "disable_skipped", r.DisableSkipped != "")
}
