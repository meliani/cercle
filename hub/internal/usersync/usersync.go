// Package usersync mirrors Navidrome's user list into the hub so that new
// users are in the `everyone` circle before they ever log in or play, and
// users deleted from Navidrome disappear (disabled, data kept).
package usersync

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/meliani/cercle/hub/internal/auth"
	"github.com/meliani/cercle/hub/internal/db"
	"github.com/meliani/cercle/hub/internal/live"
	"github.com/meliani/cercle/hub/internal/users"
	"github.com/meliani/cercle/hub/internal/visibility"
)

// Syncer runs the sync. The admin password is only ever used to derive a
// salted token per call; it is never logged nor sent as p=.
type Syncer struct {
	Nav           auth.UserLister
	AdminUser     string
	AdminPassword string
	DB            *db.DB
	Users         *users.Service
	Vis           *visibility.V
	Live          *live.Service
	Now           func() time.Time
	Log           *slog.Logger
}

// Result summarises one run.
type Result struct {
	Seen, Created, Reenabled, Disabled, Excluded int
}

// Sync runs once. On any Navidrome error nothing is changed.
func (s *Syncer) Sync(ctx context.Context) (Result, error) {
	var r Result
	list, err := s.Nav.ListUsers(ctx, auth.TokenCreds(s.AdminUser, s.AdminPassword))
	if err != nil {
		return r, err
	}
	if len(list) == 0 {
		// There is always at least the admin; an empty list is a glitch, and
		// acting on it would disable everybody.
		return r, errors.New("getUsers returned an empty list; skipping")
	}
	r.Seen = len(list)
	present := make(map[string]bool, len(list))
	for _, u := range list {
		present[strings.ToLower(u.Username)] = true
	}

	// Current hub users, to find the ones Navidrome no longer has.
	type hubUser struct {
		name     string
		disabled bool
	}
	var existing []hubUser
	rows, err := s.DB.R.QueryContext(ctx, `SELECT username, disabled_at IS NOT NULL FROM users`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var h hubUser
		if err := rows.Scan(&h.name, &h.disabled); err != nil {
			rows.Close()
			return r, err
		}
		existing = append(existing, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return r, err
	}
	wasDisabled := map[string]bool{}
	var toDisable []string
	for _, h := range existing {
		if h.disabled {
			wasDisabled[h.name] = true
		} else if !present[strings.ToLower(h.name)] {
			toDisable = append(toDisable, h.name)
		}
	}
	// Who might currently see a live state that is about to vanish: computed
	// before disabling, since a disabled user has no visible circle mates.
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

	now := s.Now().UnixMilli()
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		for _, nu := range list {
			if s.Users.IsExcluded(nu.Username) {
				r.Excluded++
				continue
			}
			admin := nu.IsAdmin
			created, err := s.Users.Ensure(ctx, tx, nu.Username, users.EnsureOpts{Name: nu.Name, IsAdmin: &admin})
			if err != nil {
				return err
			}
			if created {
				r.Created++
			} else if wasDisabled[nu.Username] {
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
			s.Log.Warn("navidrome user sync failed", "err", err)
		}
		return
	}
	s.Log.Info("navidrome user sync", "seen", r.Seen, "created", r.Created, "reenabled", r.Reenabled,
		"disabled", r.Disabled, "excluded", r.Excluded)
}
